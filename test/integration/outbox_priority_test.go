//go:build integration

package integration_test

import (
	"context"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/hibiken/asynq"

	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// itBulkEvent stands in for the mass-backfill event that caused the production
// incident (acquisition.court_record_observed): priorityFor classifies it 1
// (background). itInteractiveEvent stands in for the event a user is staring at a
// screen waiting for (draft.generation_requested): priorityFor classifies it 0
// (interactive).
type itBulkEvent struct {
	events.Base
}

func (itBulkEvent) AggregateType() string { return "outbox_priority_test" }
func (itBulkEvent) Type() string          { return "acquisition.court_record_observed" }

type itInteractiveEvent struct {
	events.Base
}

func (itInteractiveEvent) AggregateType() string { return "outbox_priority_test" }
func (itInteractiveEvent) Type() string          { return "draft.generation_requested" }

// newTestOutboxRelay wires an Outbox + Relay against the shared Postgres container and
// a real asynq client backed by an in-process miniredis — no external Redis, but the
// real enqueue plumbing (same pattern as TestRelay_ProcessAt in outbox_process_at_test.go).
func newTestOutboxRelay(t *testing.T) (*events.Outbox, database.UnitOfWork, *events.Relay) {
	t.Helper()

	pool := newPool(t)
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	client := asynq.NewClient(asynq.RedisClientOpt{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	uow := database.NewUnitOfWork(pool)
	return events.NewOutbox(), uow, events.NewRelay(uow, client)
}

// drainAll runs Tick until it returns an empty batch (the production relay loop's
// termination condition), returning every published batch in tick order so a test can
// inspect both the FIRST batch (what got prioritized) and the full drain (nothing lost).
func drainAll(t *testing.T, ctx context.Context, relay *events.Relay) [][]events.PublishedEvent {
	t.Helper()

	var batches [][]events.PublishedEvent
	for i := 0; ; i++ {
		if i >= 1000 {
			t.Fatal("relay did not drain the outbox within 1000 ticks")
		}
		pub, err := relay.Tick(ctx)
		if err != nil {
			t.Fatalf("relay tick %d: %v", i, err)
		}
		if len(pub) == 0 {
			break
		}
		batches = append(batches, pub)
	}
	return batches
}

// TestOutboxPriority_InteractiveJumpsAheadOfBulkBackfill is the acceptance criterion for
// the outbox priority fatia: it reproduces the production incident where a ~10,000-row
// DJEN backfill (bulk, priority 1) starved draft.generation_requested (interactive,
// priority 0) for 8+ minutes under the old strict ORDER BY id drain.
//
// It inserts 250 bulk rows FIRST (so they get the LOWEST ids), then ONE interactive row
// LAST (so it gets the HIGHEST id) — the worst case for a plain id-ordered drain. It then
// proves the interactive row is in the FIRST Tick batch, ahead of every bulk row, despite
// its id being larger: selectUnpublished's ORDER BY priority, id puts priority 0 first
// regardless of id.
func TestOutboxPriority_InteractiveJumpsAheadOfBulkBackfill(t *testing.T) {
	ctx := context.Background()
	outbox, uow, relay := newTestOutboxRelay(t)
	runID := uuid.NewString()

	// 10,000 — the same order of magnitude as the production incident's DJEN backfill
	// that starved draft.generation_requested. Below a few thousand rows the planner
	// reasonably prefers a Seq Scan + Sort over the index (the table is small enough
	// that the index isn't worth it) — that is correct Postgres behavior, not a
	// regression, but it means the sargability claim below only holds — and only needs
	// to hold — at realistic bulk-backfill volume.
	const bulkCount = 10000
	bulkIDs := make([]string, bulkCount)
	bulkEvents := make([]events.Event, bulkCount)
	for i := range bulkEvents {
		id := runID + "-bulk-" + uuid.NewString()
		bulkIDs[i] = id
		bulkEvents[i] = itBulkEvent{Base: events.Base{EventID: id, Aggregate: uuid.NewString()}}
	}
	if err := uow.DoSystem(ctx, func(tx database.Tx) error {
		return outbox.PublishBatch(ctx, tx, bulkEvents)
	}); err != nil {
		t.Fatalf("publish bulk batch: %v", err)
	}

	interactiveID := runID + "-interactive"
	interactive := itInteractiveEvent{Base: events.Base{EventID: interactiveID, Aggregate: uuid.NewString()}}
	if err := uow.DoSystem(ctx, func(tx database.Tx) error {
		return outbox.Publish(ctx, tx, interactive)
	}); err != nil {
		t.Fatalf("publish interactive event: %v", err)
	}

	// EXPLAIN the exact drain query (mirrors lib/events.selectUnpublished) while the rows
	// are still pending, proving the composite partial index (priority, id) WHERE
	// published_at IS NULL makes the drain an index-order scan — no "Sort" node — even
	// with 250+ pending rows. This is the "sargable" property the architect required
	// instead of ORDER BY CASE ... THEN 0 ELSE 1 END, id.
	//
	// ANALYZE first: on a freshly-loaded table the planner has no stats yet and
	// under-estimates row count, which can make it pick a Bitmap Heap Scan + explicit
	// Sort instead of the plain ordered Index Scan — a stale-stats artifact, not a
	// property of the index. Production tables get ANALYZE from autovacuum; this
	// mirrors that steady state instead of asserting on a cold, unrepresentative plan.
	pool := newPool(t)
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, "ANALYZE outbox"); err != nil {
		t.Fatalf("analyze outbox: %v", err)
	}
	rows, err := pool.Query(ctx, `EXPLAIN SELECT id, type, payload, idempotency_key, trace_context, aggregate_id, process_at
		FROM outbox
		WHERE published_at IS NULL
		ORDER BY priority, id
		LIMIT 200
		FOR UPDATE SKIP LOCKED`)
	if err != nil {
		t.Fatalf("explain drain query: %v", err)
	}
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			rows.Close()
			t.Fatalf("scan explain line: %v", err)
		}
		plan.WriteString(line)
		plan.WriteByte('\n')
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("explain rows: %v", err)
	}
	if strings.Contains(plan.String(), "Sort") {
		t.Errorf("drain query plan contains a Sort node, want an index-order scan:\n%s", plan.String())
	}

	batches := drainAll(t, ctx, relay)
	if len(batches) == 0 {
		t.Fatal("drainAll returned no batches")
	}
	firstBatch := batches[0]

	interactiveIdx := -1
	firstBulkIdx := -1
	for i, ev := range firstBatch {
		if ev.ID == interactiveID {
			interactiveIdx = i
		}
		if firstBulkIdx == -1 && strings.Contains(ev.ID, "-bulk-") && strings.HasPrefix(ev.ID, runID) {
			firstBulkIdx = i
		}
	}
	if interactiveIdx == -1 {
		t.Fatalf("interactive event %s not found in first batch (size %d)", interactiveID, len(firstBatch))
	}
	if firstBulkIdx == -1 {
		t.Fatalf("no bulk event from this run found in first batch (size %d) — batch too small to prove starvation is avoided", len(firstBatch))
	}
	if interactiveIdx >= firstBulkIdx {
		t.Errorf("interactive event at index %d, first bulk event at index %d — interactive did NOT jump ahead despite its id being LARGER", interactiveIdx, firstBulkIdx)
	}
	// NOT asserted: interactiveIdx == 0. This package shares one Postgres container
	// across every integration test (see setup_test.go's TestMain), and other tests in
	// the suite (e.g. draft_generate_test.go) publish their own draft.generation_requested
	// rows without necessarily draining them via the relay — those are also priority 0
	// and legitimately sort ahead of ours. What this test must prove is relative to OUR
	// OWN 10,000-row bulk backfill: the interactive row jumps ahead of it, which the
	// assertion above already establishes.
}

// TestOutboxPriority_FIFOWithinPriority proves priority ordering does NOT break FIFO
// within a priority class: bulk (P1) events published in a known id order drain in that
// same order, and interactive (P0) events published in a known id order drain in that
// same order too — priority only reorders ACROSS classes, never within one.
func TestOutboxPriority_FIFOWithinPriority(t *testing.T) {
	ctx := context.Background()
	outbox, uow, relay := newTestOutboxRelay(t)
	runID := uuid.NewString()

	// Published one at a time (not PublishBatch) so each gets a strictly increasing id
	// from its own INSERT, in the exact order below.
	bulkIDs := []string{runID + "-bulk-0", runID + "-bulk-1", runID + "-bulk-2"}
	for _, id := range bulkIDs {
		ev := itBulkEvent{Base: events.Base{EventID: id, Aggregate: uuid.NewString()}}
		if err := uow.DoSystem(ctx, func(tx database.Tx) error {
			return outbox.Publish(ctx, tx, ev)
		}); err != nil {
			t.Fatalf("publish bulk %s: %v", id, err)
		}
	}

	interactiveIDs := []string{runID + "-int-0", runID + "-int-1", runID + "-int-2"}
	for _, id := range interactiveIDs {
		ev := itInteractiveEvent{Base: events.Base{EventID: id, Aggregate: uuid.NewString()}}
		if err := uow.DoSystem(ctx, func(tx database.Tx) error {
			return outbox.Publish(ctx, tx, ev)
		}); err != nil {
			t.Fatalf("publish interactive %s: %v", id, err)
		}
	}

	// Concatenate every batch in tick order: ticks run sequentially, so the overall
	// relative order across all batches is the drain order, regardless of how the
	// LIMIT 200 pagination happened to split it.
	var drained []events.PublishedEvent
	for _, batch := range drainAll(t, ctx, relay) {
		drained = append(drained, batch...)
	}

	var gotBulk, gotInteractive []string
	for _, ev := range drained {
		if strings.HasPrefix(ev.ID, runID+"-bulk-") {
			gotBulk = append(gotBulk, ev.ID)
		}
		if strings.HasPrefix(ev.ID, runID+"-int-") {
			gotInteractive = append(gotInteractive, ev.ID)
		}
	}

	if !equalStrings(gotBulk, bulkIDs) {
		t.Errorf("bulk (P1) drain order = %v, want %v (FIFO by id within priority 1)", gotBulk, bulkIDs)
	}
	if !equalStrings(gotInteractive, interactiveIDs) {
		t.Errorf("interactive (P0) drain order = %v, want %v (FIFO by id within priority 0)", gotInteractive, interactiveIDs)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/hibiken/asynq"

	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// itScheduledEvent OPTS INTO future delivery via events.ScheduledEvent, reporting a
// fixed future time. Its aggregate id is a real uuid because outbox.aggregate_id is
// uuid NOT NULL.
type itScheduledEvent struct {
	events.Base
	at time.Time
}

func (itScheduledEvent) AggregateType() string          { return "outbox_test" }
func (itScheduledEvent) Type() string                   { return "outbox_test.scheduled" }
func (e itScheduledEvent) ProcessAt() (time.Time, bool) { return e.at, true }

// itImmediateEvent is a plain event: it does NOT implement ScheduledEvent, so
// process_at is NULL and it is delivered immediately — today's behavior.
type itImmediateEvent struct {
	events.Base
}

func (itImmediateEvent) AggregateType() string { return "outbox_test" }
func (itImmediateEvent) Type() string          { return "outbox_test.immediate" }

// TestRelay_ProcessAt proves the ETA primitive end to end against a real Postgres
// and a real asynq (over miniredis): a scheduled outbox row lands as a SCHEDULED
// asynq task at ~process_at, while a plain row lands PENDING — and both outbox rows
// are drained the same tick (the ETA lives in asynq, the outbox is not polled).
func TestRelay_ProcessAt(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)

	// A real asynq client + inspector backed by an in-process miniredis: no external
	// Redis, but the actual asynq enqueue/schedule paths and task states.
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	redisOpt := asynq.RedisClientOpt{Addr: mr.Addr()}
	client := asynq.NewClient(redisOpt)
	t.Cleanup(func() { _ = client.Close() })
	inspector := asynq.NewInspector(redisOpt)
	t.Cleanup(func() { _ = inspector.Close() })

	// process_at truncated to whole seconds: asynq scores the scheduled zset in unix
	// seconds, so the round-tripped NextProcessAt loses sub-second precision.
	at := time.Now().Add(1 * time.Hour).UTC().Truncate(time.Second)
	schedID := "outbox-it-sched-" + uuid.NewString()
	immID := "outbox-it-imm-" + uuid.NewString()

	sched := itScheduledEvent{
		Base: events.Base{EventID: schedID, Aggregate: uuid.NewString()},
		at:   at,
	}
	imm := itImmediateEvent{
		Base: events.Base{EventID: immID, Aggregate: uuid.NewString()},
	}

	// Producer half: both events written to the outbox in ONE system transaction,
	// exactly as a slice's use case would — no dual-write, the ETA rides on the row.
	outbox := events.NewOutbox()
	uow := database.NewUnitOfWork(pool)
	if err := uow.DoSystem(ctx, func(tx database.Tx) error {
		if err := outbox.Publish(ctx, tx, sched); err != nil {
			return err
		}
		return outbox.Publish(ctx, tx, imm)
	}); err != nil {
		t.Fatalf("publish to outbox: %v", err)
	}

	// Relay half: drain the outbox into asynq. Tick takes LIMIT 200 rows per pass and
	// other tests in this shared-container package may have left a larger backlog, so
	// tick until it drains dry — exactly what the production relay loop does over time.
	// queueFor("outbox_test.*") has no domain prefix, so both our tasks route to
	// "default".
	relay := events.NewRelay(uow, client)
	for i := 0; ; i++ {
		if i >= 1000 {
			t.Fatal("relay did not drain the outbox within 1000 ticks")
		}
		pub, err := relay.Tick(ctx)
		if err != nil {
			t.Fatalf("relay tick: %v", err)
		}
		if len(pub) == 0 {
			break
		}
	}

	// The scheduled event is a SCHEDULED task whose next process time is ~process_at.
	schedInfo, err := inspector.GetTaskInfo("default", schedID)
	if err != nil {
		t.Fatalf("get scheduled task info: %v", err)
	}
	if schedInfo.State != asynq.TaskStateScheduled {
		t.Errorf("scheduled task state = %v, want Scheduled", schedInfo.State)
	}
	if delta := schedInfo.NextProcessAt.Sub(at); delta < -2*time.Second || delta > 2*time.Second {
		t.Errorf("scheduled NextProcessAt = %v, want ≈ %v (delta %v)", schedInfo.NextProcessAt, at, delta)
	}

	// The plain event is a PENDING task, deliverable now — the NULL process_at path.
	immInfo, err := inspector.GetTaskInfo("default", immID)
	if err != nil {
		t.Fatalf("get immediate task info: %v", err)
	}
	if immInfo.State != asynq.TaskStatePending {
		t.Errorf("immediate task state = %v, want Pending", immInfo.State)
	}

	// Both outbox rows are drained THIS tick — published_at stamped despite the ETA.
	for _, id := range []string{schedID, immID} {
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM outbox WHERE idempotency_key = $1 AND published_at IS NULL`,
			id,
		).Scan(&n); err != nil {
			t.Fatalf("count unpublished for %s: %v", id, err)
		}
		if n != 0 {
			t.Errorf("outbox row %s still unpublished = %d, want 0 (drained same tick)", id, n)
		}
	}
}

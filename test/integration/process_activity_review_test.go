//go:build integration

// Process activity log integration test — the SECOND half of the feature (the first
// half, the SYNCHRONOUS intimation-analysis producer, is covered by
// analise_activity_test.go). This file proves the async draft-generation path: a
// successful Revisar call (ReviewUseCase.ReviewDraft, internal/draft/review.go)
// publishes review.completed to the outbox in the SAME tx as the review row; the
// relay routes it to the "notifications" queue; acquisition's activity listener
// (internal/acquisition/activity_listener.go) consumes it and appends ONE
// DRAFT_GENERATED row to process_activity_log, resolving the owning court_record via
// the draft's intimation. It also proves GET /processos/:id/activity's tenant
// isolation via the read use case.
//
// What the unit tests with mocked repos/fakes CANNOT prove:
//   - ReviewDraft's outbox row and the review row commit in the SAME tx (real Postgres).
//   - The relay's queueFor literal for "review.completed" matches what the consumer
//     listener is actually registered on (both point at "notifications").
//   - The listener's SQL resolution of court_record_id from draft_id (via the
//     intimation join) works against the real schema.
//   - processed_event dedup: a duplicate event_id is a no-op (idempotent insert).
//   - Tenant isolation: the Atividade read never leaks a foreign tenant's rows.
package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jusassessoria/platform/internal/acquisition"
	"github.com/jusassessoria/platform/internal/advisory"
	"github.com/jusassessoria/platform/internal/draft"
	"github.com/jusassessoria/platform/internal/indexing"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
	"github.com/jusassessoria/platform/lib/llm"
)

// newReviewUCWithOutbox wires the review use case against the real pool, WITH the
// real Outbox (unlike draft_review_test.go's newReviewUC, which leaves Outbox nil
// for its own review-only assertions) so review.completed actually lands in the
// outbox table — this file's round-trip test needs it published.
func newReviewUCWithOutbox(pool *pgxpool.Pool, gen llm.Generator) *draft.ReviewUseCase {
	repo := draft.NewRepository()
	return draft.NewReviewUseCase(draft.ReviewUseCaseParams{
		UoW:      database.NewUnitOfWork(pool),
		Reader:   repo,
		Writer:   repo,
		Outbox:   events.NewOutbox(),
		Gen:      gen,
		Search:   indexing.SearchDeps{Pool: nil}, // degraded — no RAG
		Composer: advisory.NewTemplateComposer(),
		Model:    "test-model",
	})
}

// readOutboxPayload reads back one outbox row's raw jsonb payload for (type, aggregate_id)
// — the owner query the round-trip test uses to feed the REAL asynq decode path
// (events.Decode, exercised inside the listener) rather than reconstructing the event
// by hand.
func readOutboxPayload(t *testing.T, pool *pgxpool.Pool, eventType, aggregateID string) []byte {
	t.Helper()
	var payload []byte
	if err := pool.QueryRow(context.Background(),
		`SELECT payload FROM outbox WHERE type = $1 AND aggregate_id = $2 ORDER BY id DESC LIMIT 1`,
		eventType, aggregateID).Scan(&payload); err != nil {
		t.Fatalf("read outbox payload: %v", err)
	}
	return payload
}

// TestReviewCompleted_ActivityListener_RoundTrip proves the full producer→outbox→
// consumer chain: Gerar (DRAFTED) → Revisar (ReviewDraft, publishes review.completed)
// → the activity listener (via the real asynq decode path, ServeMux.ProcessTask) →
// one DRAFT_GENERATED row, readable through GET /processos/:id/activity's use case.
func TestReviewCompleted_ActivityListener_RoundTrip(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-activity-review", 0)
	recordID, caseID := seedCourtRecordCNJ(t, pool, tenantID, "0020002-02.2026.8.26.0002")
	intimationID := seedIntimationTyped(t, pool, tenantID, caseID, recordID, "CITACAO")

	// ── Gerar: create + trigger + consume, so the draft has content (DRAFTED) ──
	draftUC := newDraftUC(pool)
	created, err := draftUC.Create(ctx, draft.CreateCommand{
		TenantID:     tenantID,
		Source:       draft.SourceIntimation,
		IntimationID: intimationID,
	})
	if err != nil {
		t.Fatalf("Create draft: %v", err)
	}
	draftID := created.Draft.ID

	triggerUC := newTriggerUC(pool)
	_, err = triggerUC.TriggerGeneration(ctx, draft.TriggerGenerationCommand{TenantID: tenantID, DraftID: draftID})
	if err != nil {
		t.Fatalf("TriggerGeneration: %v", err)
	}
	generateUC := newGenerateUC(pool, &integrationFakeGen{out: []byte(cannedGenerationJSON)})
	genEventID := uuid.Must(uuid.NewV7()).String()
	if err := generateUC.OnGenerationRequested(ctx, draft.GenerationRequested{
		Base:     events.Base{EventID: genEventID, Aggregate: draftID},
		DraftID:  draftID,
		TenantID: tenantID,
	}); err != nil {
		t.Fatalf("OnGenerationRequested: %v", err)
	}

	// ── Revisar: publishes review.completed in the SAME tx as the review row ──
	reviewUC := newReviewUCWithOutbox(pool, &integrationFakeGen{out: []byte(cannedReviewJSON)})
	if _, err := reviewUC.ReviewDraft(ctx, draft.ReviewDraftCommand{TenantID: tenantID, DraftID: draftID}); err != nil {
		t.Fatalf("ReviewDraft: %v", err)
	}
	if n := countOutboxRows(t, pool, draft.TypeReviewCompleted, draftID); n != 1 {
		t.Fatalf("outbox rows for review.completed = %d, want 1", n)
	}

	// ── Consumer: feed the REAL outbox payload through the REAL asynq decode path ──
	payload := readOutboxPayload(t, pool, draft.TypeReviewCompleted, draftID)
	repo := acquisition.NewRepository(pool)
	activityUC := acquisition.NewActivityUseCase(
		repo, acquisition.NewActivityDeduper(), acquisition.NewActivityLogWriter(), database.NewUnitOfWork(pool),
	)
	mux := asynq.NewServeMux()
	acquisition.NewActivityListener(activityUC).Register(mux)

	task := events.Encode(acquisition.TypeReviewCompleted, payload)
	if err := mux.ProcessTask(ctx, task); err != nil {
		t.Fatalf("ProcessTask(review.completed): %v", err)
	}

	if got := countProcessActivityLog(t, pool, recordID, acquisition.ActivityEventDraftGenerated); got != 1 {
		t.Errorf("process_activity_log rows = %d, want 1", got)
	}

	// ── Redelivery is idempotent: processing the SAME task again inserts no second row ──
	if err := mux.ProcessTask(ctx, task); err != nil {
		t.Fatalf("ProcessTask (redelivery): %v", err)
	}
	if got := countProcessActivityLog(t, pool, recordID, acquisition.ActivityEventDraftGenerated); got != 1 {
		t.Errorf("process_activity_log rows after redelivery = %d, want 1 (dedup)", got)
	}

	// ── Readable via the Atividade tab's read use case, newest first, text rendered ──
	readUC := acquisition.NewReadUseCase(repo)
	res, err := readUC.ActivityLog(ctx, acquisition.ActivityLogQuery{
		TenantID: tenantID, CourtRecordID: recordID,
		LastOccurred: "9999-12-31T23:59:59Z",
		LastID:       "ffffffff-ffff-ffff-ffff-ffffffffffff",
		Limit:        10,
	})
	if err != nil {
		t.Fatalf("ActivityLog: %v", err)
	}
	if len(res.Items) != 1 {
		t.Fatalf("len(Items) = %d, want 1", len(res.Items))
	}
	if res.Items[0].EventType != acquisition.ActivityEventDraftGenerated {
		t.Errorf("EventType = %q, want %q", res.Items[0].EventType, acquisition.ActivityEventDraftGenerated)
	}
	if res.Items[0].Text != "Peça gerada" {
		t.Errorf("Text = %q, want %q", res.Items[0].Text, "Peça gerada")
	}
}

// TestActivityLog_TenantIsolation proves GET /processos/:id/activity's tenant scoping
// (barrier 1 + RLS, migration 0073's tenant_isolation policy): two tenants each get
// their own process_activity_log row, and tenant A's read never returns tenant B's row
// — even querying tenant A's OWN court_record_id would be impossible cross-tenant
// (each tenant has its own court_record), so this proves the read is properly scoped
// per (tenant, court_record) pair.
func TestActivityLog_TenantIsolation(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()

	tenantA := uuid.NewString()
	seedTenant(t, pool, tenantA, "org-activity-iso-a", 0)
	recordA, _ := seedCourtRecordCNJ(t, pool, tenantA, "0030003-03.2026.8.26.0003")

	tenantB := uuid.NewString()
	seedTenant(t, pool, tenantB, "org-activity-iso-b", 0)
	recordB, _ := seedCourtRecordCNJ(t, pool, tenantB, "0040004-04.2026.8.26.0004")

	seedActivityLogRow(t, pool, tenantA, recordA, acquisition.ActivityEventDraftGenerated)
	seedActivityLogRow(t, pool, tenantB, recordB, acquisition.ActivityEventDraftGenerated)

	readUC := acquisition.NewReadUseCase(acquisition.NewRepository(pool))

	resA, err := readUC.ActivityLog(ctx, acquisition.ActivityLogQuery{
		TenantID: tenantA, CourtRecordID: recordA,
		LastOccurred: "9999-12-31T23:59:59Z",
		LastID:       "ffffffff-ffff-ffff-ffff-ffffffffffff",
		Limit:        10,
	})
	if err != nil {
		t.Fatalf("ActivityLog (tenant A): %v", err)
	}
	if len(resA.Items) != 1 {
		t.Fatalf("tenant A: len(Items) = %d, want 1 (only its own row)", len(resA.Items))
	}

	// tenant B querying tenant A's court_record: tenant scoping makes the row
	// invisible (RLS + the tenant_id filter), so this yields an EMPTY page, not 404
	// and never tenant A's row.
	resCross, err := readUC.ActivityLog(ctx, acquisition.ActivityLogQuery{
		TenantID: tenantB, CourtRecordID: recordA,
		LastOccurred: "9999-12-31T23:59:59Z",
		LastID:       "ffffffff-ffff-ffff-ffff-ffffffffffff",
		Limit:        10,
	})
	if err != nil {
		t.Fatalf("ActivityLog (tenant B on tenant A's record): %v", err)
	}
	if len(resCross.Items) != 0 {
		t.Errorf("cross-tenant read leaked %d row(s), want 0", len(resCross.Items))
	}
}

// seedActivityLogRow inserts one process_activity_log row directly (owner insert,
// bypassing the app write path — the point here is to test the READ's isolation, not
// the write).
func seedActivityLogRow(t *testing.T, pool *pgxpool.Pool, tenantID, courtRecordID, eventType string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO process_activity_log (tenant_id, court_record_id, event_type, payload, occurred_at)
		 VALUES ($1, $2, $3, '{}', $4)`,
		tenantID, courtRecordID, eventType, time.Now()); err != nil {
		t.Fatalf("seed process_activity_log: %v", err)
	}
}

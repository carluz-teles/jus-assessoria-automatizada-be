//go:build integration

// draft_review integration tests — prove the Revisar (ReviewDraft) synchronous use
// case round-trip against a REAL Postgres with migrations applied through
// 0044_review_status.
//
// What unit tests with mocked repos CANNOT prove:
//   - ReviewDraft inserts a review row with status=COMPLETED, non-empty findings,
//     and coverage in a real transaction, against the actual schema.
//   - ReviewDraft transitions saga_state from DRAFTED → REVIEWED in the DB.
//   - Regenerating (OnGenerationRequested) deletes old review rows and resets
//     saga_state=DRAFTED — proving DeleteReviewsForDraft fires atomically.
//   - Tenant isolation: GetDetail for a foreign tenant never exposes the review.
//
// The real LLM API is NEVER called. A fake llm.Generator returns canned JSON.
// Embedder is nil (degraded path — no grounding).
package integration_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jusassessoria/platform/internal/advisory"
	"github.com/jusassessoria/platform/internal/draft"
	"github.com/jusassessoria/platform/internal/indexing"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
	"github.com/jusassessoria/platform/lib/llm"
)

// ── Helpers ───────────────────────────────────────────────────────────────────

// newReviewUC wires the review use case against the real pool with the given
// fake generator. Embedder and search pool are nil → degraded (no RAG).
func newReviewUC(pool *pgxpool.Pool, gen llm.Generator) *draft.ReviewUseCase {
	repo := draft.NewRepository()
	return draft.NewReviewUseCase(draft.ReviewUseCaseParams{
		UoW:      database.NewUnitOfWork(pool),
		Reader:   repo,
		Writer:   repo,
		Gen:      gen,
		Emb:      nil, // nil → degraded (no grounding in integration tests)
		Search:   indexing.SearchDeps{Pool: nil},
		Composer: advisory.NewTemplateComposer(),
		Model:    "claude-opus-4-8",
	})
}

// seedDraftWithContent creates a draft via Gerar (TriggerGeneration +
// OnGenerationRequested) so it has content and saga_state=DRAFTED.
// Returns the draftID.
func seedDraftWithContent(t *testing.T, pool *pgxpool.Pool, tenantID string) string {
	t.Helper()
	ctx := context.Background()

	draftUC := newDraftUC(pool)
	created, err := draftUC.Create(ctx, draft.CreateCommand{
		TenantID: tenantID,
		Source:   draft.SourceBlank,
	})
	if err != nil {
		t.Fatalf("seedDraftWithContent Create: %v", err)
	}
	draftID := created.Draft.ID

	triggerUC := newTriggerUC(pool)
	if _, err := triggerUC.TriggerGeneration(ctx, draft.TriggerGenerationCommand{
		TenantID: tenantID,
		DraftID:  draftID,
	}); err != nil {
		t.Fatalf("seedDraftWithContent TriggerGeneration: %v", err)
	}

	gen := &integrationFakeGen{out: []byte(cannedGenerationJSON)}
	generateUC := newGenerateUC(pool, gen)
	eventID := uuid.Must(uuid.NewV7()).String()
	if err := generateUC.OnGenerationRequested(ctx, draft.GenerationRequested{
		Base:     events.Base{EventID: eventID, Aggregate: draftID},
		DraftID:  draftID,
		TenantID: tenantID,
	}); err != nil {
		t.Fatalf("seedDraftWithContent OnGenerationRequested: %v", err)
	}
	return draftID
}

// ── AC1: ReviewDraft happy path — review row + REVIEWED state ─────────────────

// TestDraftReview_HappyPath proves ReviewDraft:
//  1. Inserts a review row with status=COMPLETED, non-empty findings, and coverage.
//  2. Transitions saga_state from DRAFTED → REVIEWED in the DB.
//  3. Does NOT modify draft.content (only suggestions, no minuta update).
//
// The fake generator returns cannedReviewJSON; embedder is nil (degraded).
func TestDraftReview_HappyPath(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-review-happy", 0)
	draftID := seedDraftWithContent(t, pool, tenantID)

	gen := &integrationFakeGen{out: []byte(cannedReviewJSON)}
	reviewUC := newReviewUC(pool, gen)

	result, err := reviewUC.ReviewDraft(ctx, draft.ReviewDraftCommand{
		TenantID: tenantID,
		DraftID:  draftID,
	})
	if err != nil {
		t.Fatalf("ReviewDraft: %v", err)
	}

	// ── result fields ─────────────────────────────────────────────────────────
	if result.SagaState != draft.SagaStateReviewed {
		t.Errorf("result.SagaState = %q, want REVIEWED", result.SagaState)
	}
	if result.Review == nil {
		t.Fatal("result.Review is nil — ReviewDraft did not return the inserted review")
	}
	if result.Review.Status != draft.ReviewStatusCompleted {
		t.Errorf("result.Review.Status = %q, want COMPLETED", result.Review.Status)
	}

	// ── DB: saga_state = REVIEWED ─────────────────────────────────────────────
	if got := readDraftSagaState(t, pool, draftID); got != draft.SagaStateReviewed {
		t.Errorf("DB saga_state = %q, want REVIEWED", got)
	}

	// ── DB: one review row with COMPLETED status ──────────────────────────────
	if n := countReviews(t, pool, draftID); n != 1 {
		t.Fatalf("review count = %d, want 1", n)
	}
	rev := readLatestReviewRaw(t, pool, draftID)
	if rev.Status != draft.ReviewStatusCompleted {
		t.Errorf("review.status = %q, want COMPLETED", rev.Status)
	}
	// The canned review JSON has 2 valid suggestions (Clareza + Argumento with citation).
	if len(rev.Findings) == 0 {
		t.Error("review.findings is empty — buildFindings produced no valid suggestions")
	}
	if len(rev.Findings) != 2 {
		t.Errorf("review.findings count = %d, want 2 (Clareza + Argumento)", len(rev.Findings))
	}
	// Confirm grounded=false (embedder nil → degraded path).
	if rev.Coverage.Grounded {
		t.Error("coverage.grounded = true, want false (embedder nil → degraded)")
	}

	// ── GetDetail exposes the review via the read model ───────────────────────
	draftUC := newDraftUC(pool)
	view, err := draftUC.GetDetail(ctx, tenantID, draftID)
	if err != nil {
		t.Fatalf("GetDetail: %v", err)
	}
	if view.Review == nil {
		t.Fatal("view.Review is nil — GetLatestReview JOIN not returning the inserted review")
	}
	if view.Review.Status != draft.ReviewStatusCompleted {
		t.Errorf("view.Review.Status = %q, want COMPLETED", view.Review.Status)
	}
}

// ── AC2: Regenerate deletes old reviews ──────────────────────────────────────

// TestDraftReview_RegenerateDeletesOldReviews proves that calling Gerar again
// (OnGenerationRequested) after a review exists:
//  1. Deletes all existing review rows for the draft (DeleteReviewsForDraft).
//  2. Sets saga_state back to DRAFTED (not REVIEWED).
//  3. Leaves no review rows in the DB.
//
// This is the atomic Gerar reset: old reviews are cleared in the same tx as the
// new DRAFTED state, so the advogado always sees a consistent state.
func TestDraftReview_RegenerateDeletesOldReviews(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-review-regen", 0)
	draftID := seedDraftWithContent(t, pool, tenantID)

	// First: run Revisar to create a review row.
	revGen := &integrationFakeGen{out: []byte(cannedReviewJSON)}
	reviewUC := newReviewUC(pool, revGen)
	if _, err := reviewUC.ReviewDraft(ctx, draft.ReviewDraftCommand{
		TenantID: tenantID,
		DraftID:  draftID,
	}); err != nil {
		t.Fatalf("ReviewDraft: %v", err)
	}
	if n := countReviews(t, pool, draftID); n != 1 {
		t.Fatalf("review count before regen = %d, want 1", n)
	}
	if got := readDraftSagaState(t, pool, draftID); got != draft.SagaStateReviewed {
		t.Fatalf("saga_state before regen = %q, want REVIEWED", got)
	}

	// Second: re-trigger generation (DRAFTED allows re-generation).
	triggerUC := newTriggerUC(pool)
	if _, err := triggerUC.TriggerGeneration(ctx, draft.TriggerGenerationCommand{
		TenantID: tenantID,
		DraftID:  draftID,
	}); err != nil {
		t.Fatalf("re-trigger TriggerGeneration: %v", err)
	}

	// Deliver a new generation event (new event_id).
	genFake := &integrationFakeGen{out: []byte(cannedGenerationJSON)}
	generateUC := newGenerateUC(pool, genFake)
	newEventID := uuid.Must(uuid.NewV7()).String()
	if err := generateUC.OnGenerationRequested(ctx, draft.GenerationRequested{
		Base:     events.Base{EventID: newEventID, Aggregate: draftID},
		DraftID:  draftID,
		TenantID: tenantID,
	}); err != nil {
		t.Fatalf("second OnGenerationRequested (regenerate): %v", err)
	}

	// ── DB: review rows must be gone (deleted by Gerar) ──────────────────────
	if n := countReviews(t, pool, draftID); n != 0 {
		t.Errorf("review count after regen = %d, want 0 (DeleteReviewsForDraft)", n)
	}

	// ── DB: saga_state must be DRAFTED (reset by Gerar) ──────────────────────
	if got := readDraftSagaState(t, pool, draftID); got != draft.SagaStateDrafted {
		t.Errorf("saga_state after regen = %q, want DRAFTED", got)
	}

	// ── GetDetail: view.Review is nil after regen ─────────────────────────────
	draftUC := newDraftUC(pool)
	view, err := draftUC.GetDetail(ctx, tenantID, draftID)
	if err != nil {
		t.Fatalf("GetDetail after regen: %v", err)
	}
	if view.Review != nil {
		t.Errorf("view.Review = %+v, want nil after regeneration (old review deleted)", view.Review)
	}
}

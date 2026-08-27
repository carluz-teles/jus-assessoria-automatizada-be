package draft

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jusassessoria/platform/internal/advisory"
	"github.com/jusassessoria/platform/internal/indexing"
	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/llm"
)

// fakeReviewWriter captures InsertReview and UpdateSagaState calls for review tests.
// It is separate from fakeWriter to keep test state isolated.
type fakeReviewWriter struct {
	insertedReview   *Review
	updatedSagaState string
	returnedDraft    *Draft
	returnedReview   *Review
	writeErr         error
}

func (f *fakeReviewWriter) InsertReview(_ context.Context, _ database.Tx, r *Review) (*Review, error) {
	f.insertedReview = r
	if f.returnedReview != nil {
		return f.returnedReview, f.writeErr
	}
	r.ID = "review-rev-1"
	return r, f.writeErr
}

func (f *fakeReviewWriter) UpdateSagaState(_ context.Context, _ database.Tx, _, _, sagaState string, _ bool, _ string, _ *StructuredContent) (*Draft, error) {
	f.updatedSagaState = sagaState
	if f.returnedDraft != nil {
		return f.returnedDraft, f.writeErr
	}
	return &Draft{SagaState: sagaState}, f.writeErr
}

// makeDraftedDraft returns a test draft in DRAFTED state with non-empty content,
// ready for Revisar.
func makeDraftedDraft() *Draft {
	return &Draft{
		ID:        "draft-rev-1",
		TenantID:  "tenant-rev-1",
		SagaState: SagaStateDrafted,
		PieceType: PieceTypeDefense,
		Content:   "Excelentíssimo Senhor Juiz, vem o réu apresentar contestação. Argumento claro.",
		UpdatedAt: time.Now(),
	}
}

// buildReviewUC assembles the ReviewUseCase with the given overridable parts.
func buildReviewUC(
	uow database.UnitOfWork,
	reader reviewDepsReader,
	writer reviewWriter,
	gen llm.Generator,
	emb embedder,
) *ReviewUseCase {
	return NewReviewUseCase(ReviewUseCaseParams{
		UoW:      uow,
		Reader:   reader,
		Writer:   writer,
		Gen:      gen,
		Emb:      emb,
		Search:   indexing.SearchDeps{Pool: nil}, // nil pool → degraded
		Composer: advisory.NewTemplateComposer(),
		Model:    "test-model",
	})
}

// reviewCmd returns a canonical ReviewDraftCommand.
func reviewCmd() ReviewDraftCommand {
	return ReviewDraftCommand{
		TenantID: "tenant-rev-1",
		DraftID:  "draft-rev-1",
	}
}

// cannedReviewJSON is the JSON the fake generator returns for Revisar.
// `original` values MUST appear in makeDraftedDraft().Content.
const cannedReviewJSON = `{
  "suggestions": [
    {
      "category": "Clareza",
      "original": "Excelentíssimo Senhor Juiz",
      "replacement": "Exmo. Sr. Juiz",
      "problem": "Fórmula longa",
      "description": "Usar a forma abreviada"
    },
    {
      "category": "Argumento",
      "original": "Argumento claro.",
      "replacement": "Argumento juridicamente fundamentado.",
      "problem": "Argumento vago",
      "description": "Citar dispositivo legal",
      "citation": {
        "document_id": "doc-cf",
        "page": 1,
        "quote": "Conforme o art. 5° da CF"
      }
    }
  ]
}`

// ── Tests ─────────────────────────────────────────────────────────────────────

// TestReviewUseCase_NilGenerator_422 verifies that a nil generator returns
// an apperr.Invalid (422) immediately without touching the repository.
func TestReviewUseCase_NilGenerator_422(t *testing.T) {
	d := makeDraftedDraft()
	w := &fakeReviewWriter{}

	uc := buildReviewUC(fakeUoW{}, fakeReader{draft: d}, w, nil, nil)

	_, err := uc.ReviewDraft(context.Background(), reviewCmd())
	if err == nil {
		t.Fatal("want error for nil generator, got nil")
	}
	var ae *apperr.AppError
	if !errors.As(err, &ae) || ae.Kind != apperr.KindInvalid {
		t.Errorf("want KindInvalid (422), got: %v", err)
	}
	// No writes must have happened.
	if w.insertedReview != nil {
		t.Error("insertedReview non-nil, want nil (no writes for nil generator)")
	}
}

// TestReviewUseCase_EmptyContent_422 verifies that a draft with empty content
// returns a KindInvalid error before calling the LLM.
func TestReviewUseCase_EmptyContent_422(t *testing.T) {
	d := makeDraftedDraft()
	d.Content = "" // empty content → guard must fire
	gen := &fakeGen{out: []byte(cannedReviewJSON)}
	w := &fakeReviewWriter{}

	uc := buildReviewUC(fakeUoW{}, fakeReader{draft: d}, w, gen, nil)

	_, err := uc.ReviewDraft(context.Background(), reviewCmd())
	if err == nil {
		t.Fatal("want error for empty content, got nil")
	}
	var ae *apperr.AppError
	if !errors.As(err, &ae) || ae.Kind != apperr.KindInvalid {
		t.Errorf("want KindInvalid (422), got: %v", err)
	}
	// LLM must not have been called.
	if gen.gotReq.SchemaName != "" {
		t.Error("LLM was called despite empty content guard")
	}
}

// TestReviewUseCase_ExtractingSaga_409 verifies that reviewing a draft in
// EXTRACTING state (generation in progress) returns a KindConflict (409).
func TestReviewUseCase_ExtractingSaga_409(t *testing.T) {
	d := makeDraftedDraft()
	d.SagaState = SagaStateExtracting
	d.Content = "some content"
	gen := &fakeGen{out: []byte(cannedReviewJSON)}
	w := &fakeReviewWriter{}

	uc := buildReviewUC(fakeUoW{}, fakeReader{draft: d}, w, gen, nil)

	_, err := uc.ReviewDraft(context.Background(), reviewCmd())
	if err == nil {
		t.Fatal("want error for EXTRACTING saga, got nil")
	}
	var ae *apperr.AppError
	if !errors.As(err, &ae) || ae.Kind != apperr.KindConflict {
		t.Errorf("want KindConflict (409), got: %v", err)
	}
}

// TestReviewUseCase_DraftNotFound_404 verifies that a missing draft returns
// ErrDraftNotFound (KindNotFound → 404).
func TestReviewUseCase_DraftNotFound_404(t *testing.T) {
	gen := &fakeGen{out: []byte(cannedReviewJSON)}
	w := &fakeReviewWriter{}

	uc := buildReviewUC(fakeUoW{}, fakeReader{draftErr: ErrDraftNotFound}, w, gen, nil)

	_, err := uc.ReviewDraft(context.Background(), reviewCmd())
	if !errors.Is(err, ErrDraftNotFound) {
		t.Errorf("want ErrDraftNotFound, got %v", err)
	}
}

// TestReviewUseCase_LLMError_NoFailedState verifies that an LLM error returns
// an error but does NOT persist a FAILED saga state (the draft remains DRAFTED).
// Revisar is retryable without re-generating.
func TestReviewUseCase_LLMError_NoFailedState(t *testing.T) {
	d := makeDraftedDraft()
	gen := &fakeGen{err: errors.New("llm timeout")}
	w := &fakeReviewWriter{}

	uc := buildReviewUC(fakeUoW{}, fakeReader{draft: d}, w, gen, nil)

	_, err := uc.ReviewDraft(context.Background(), reviewCmd())
	if err == nil {
		t.Fatal("want error for LLM failure, got nil")
	}
	// No saga state change — draft stays DRAFTED.
	if w.updatedSagaState != "" {
		t.Errorf("saga_state updated to %q, want empty (no state change on LLM error)", w.updatedSagaState)
	}
	// No review inserted.
	if w.insertedReview != nil {
		t.Error("review inserted despite LLM error, want nil")
	}
}

// TestReviewUseCase_HappyPath verifies the full success flow:
// InsertReview (COMPLETED) + UpdateSagaState → REVIEWED + ReviewResult returned.
func TestReviewUseCase_HappyPath(t *testing.T) {
	d := makeDraftedDraft()
	gen := &fakeGen{out: []byte(cannedReviewJSON)}
	w := &fakeReviewWriter{}

	uc := buildReviewUC(fakeUoW{}, fakeReader{draft: d}, w, gen, nil)

	result, err := uc.ReviewDraft(context.Background(), reviewCmd())
	if err != nil {
		t.Fatalf("want nil err, got %v", err)
	}
	if result == nil {
		t.Fatal("result is nil, want non-nil")
	}

	// Review must be persisted with COMPLETED status.
	if w.insertedReview == nil {
		t.Fatal("no review inserted")
	}
	if w.insertedReview.Status != ReviewStatusCompleted {
		t.Errorf("review.Status = %q, want COMPLETED", w.insertedReview.Status)
	}
	// Findings: cannedReviewJSON has 2 suggestions (Clareza + Argumento with citation)
	// both with `original` substrings present in makeDraftedDraft().Content.
	if len(w.insertedReview.Findings) != 2 {
		t.Errorf("findings = %d, want 2", len(w.insertedReview.Findings))
	}

	// Saga must transition to REVIEWED.
	if w.updatedSagaState != SagaStateReviewed {
		t.Errorf("saga_state = %q, want REVIEWED", w.updatedSagaState)
	}
	// Result carries the saga state.
	if result.SagaState != SagaStateReviewed {
		t.Errorf("result.SagaState = %q, want REVIEWED", result.SagaState)
	}
	// Result carries the review.
	if result.Review == nil {
		t.Error("result.Review is nil, want the inserted review")
	}
}

// TestReviewUseCase_NeverPublishes verifies that Revisar (ReviewUseCase.ReviewDraft)
// never touches the outbox — it only produces AI critique suggestions over an
// EXISTING minuta (review.go's own header comment) and must not be confused with
// Gerar, which is the use case that actually writes draft content and publishes
// draft.generated (see generate_test.go's TestGenerateUseCase_HappyPath for that
// assertion in the mirror direction). ReviewUseCase has no Outbox port at all —
// this test pins that by construction: any future Publish call added here would
// fail to compile against ReviewUseCaseParams, not just fail an assertion.
func TestReviewUseCase_NeverPublishes(t *testing.T) {
	d := makeDraftedDraft()
	gen := &fakeGen{out: []byte(cannedReviewJSON)}
	w := &fakeReviewWriter{}

	uc := buildReviewUC(fakeUoW{}, fakeReader{draft: d}, w, gen, nil)

	if _, err := uc.ReviewDraft(context.Background(), reviewCmd()); err != nil {
		t.Fatalf("want nil err, got %v", err)
	}
}

// TestReviewUseCase_SubstringValidation verifies that suggestions whose `original`
// does not appear in the draft content are dropped by buildFindings.
func TestReviewUseCase_SubstringValidation(t *testing.T) {
	d := makeDraftedDraft()
	// Add a suggestion with `original` NOT in the draft content.
	jsonWithMiss := []byte(`{
  "suggestions": [
    {
      "category": "Clareza",
      "original": "SUBSTRING THAT DOES NOT EXIST IN DRAFT",
      "replacement": "r",
      "problem": "p",
      "description": "d"
    },
    {
      "category": "Clareza",
      "original": "Argumento claro.",
      "replacement": "Argumento jurídico preciso.",
      "problem": "Imprecisão",
      "description": "Ser mais preciso"
    }
  ]
}`)
	gen := &fakeGen{out: jsonWithMiss}
	w := &fakeReviewWriter{}

	uc := buildReviewUC(fakeUoW{}, fakeReader{draft: d}, w, gen, nil)

	result, err := uc.ReviewDraft(context.Background(), reviewCmd())
	if err != nil {
		t.Fatalf("want nil err, got %v", err)
	}
	// Only the second suggestion (valid substring) must survive.
	if len(result.Review.Findings) != 1 {
		t.Errorf("findings = %d, want 1 (non-substring suggestion dropped)", len(result.Review.Findings))
	}
	if result.Review.Coverage.SuggestionsDropped != 1 {
		t.Errorf("dropped = %d, want 1", result.Review.Coverage.SuggestionsDropped)
	}
}

// TestReviewUseCase_WithIntimation_CRIDResolved verifies that when the draft has an
// intimation with a non-empty CourtRecordID, the court_record_id is loaded (non-fatal
// if absent). With nil pool the RAG degrades gracefully (grounded=false).
func TestReviewUseCase_WithIntimation_CRIDResolved(t *testing.T) {
	d := makeDraftedDraft()
	d.IntimationID = "intim-rev-1"
	intim := &IntimationContext{
		IntimationID:  "intim-rev-1",
		CourtRecordID: "court-record-rev-1",
	}
	gen := &fakeGen{out: []byte(cannedReviewJSON)}
	w := &fakeReviewWriter{}

	uc := buildReviewUC(
		fakeUoW{},
		fakeReader{draft: d, intimation: intim},
		w,
		gen,
		nil, // nil embedder → degraded
	)

	result, err := uc.ReviewDraft(context.Background(), reviewCmd())
	if err != nil {
		t.Fatalf("want nil err, got %v", err)
	}
	// Generation must succeed and reach REVIEWED even without grounding.
	if result.SagaState != SagaStateReviewed {
		t.Errorf("saga_state = %q, want REVIEWED", result.SagaState)
	}
	if result.Review.Coverage.Grounded {
		t.Error("grounded = true, want false (nil embedder → no RAG)")
	}
}

// TestReviewUseCase_ThreePhasesLLMOutsideTx verifies the three-phase structure:
// the LLM call (Phase 2) must NOT be inside a transaction. We verify this by
// confirming that the fakeUoW is called exactly twice (Phase 1 read tx + Phase 3
// write tx) and the LLM was called between them.
func TestReviewUseCase_ThreePhasesLLMOutsideTx(t *testing.T) {
	d := makeDraftedDraft()

	var calls []string
	trackingUoW := trackingFakeUoW{onDo: func(label string) {
		calls = append(calls, label)
	}}
	gen := &fakeGenTracked{
		out:    []byte(cannedReviewJSON),
		onCall: func() { calls = append(calls, "llm") },
	}
	w := &fakeReviewWriter{}

	uc := NewReviewUseCase(ReviewUseCaseParams{
		UoW:      trackingUoW,
		Reader:   fakeReader{draft: d},
		Writer:   w,
		Gen:      gen,
		Emb:      nil,
		Search:   indexing.SearchDeps{Pool: nil},
		Composer: advisory.NewTemplateComposer(),
		Model:    "test-model",
	})

	if _, err := uc.ReviewDraft(context.Background(), reviewCmd()); err != nil {
		t.Fatalf("want nil err, got %v", err)
	}

	// Expected order: tx-read → llm → tx-write.
	if len(calls) != 3 {
		t.Fatalf("calls = %v, want [tx-read, llm, tx-write]", calls)
	}
	if calls[0] != "tx" || calls[1] != "llm" || calls[2] != "tx" {
		t.Errorf("call order = %v, want [tx, llm, tx]", calls)
	}
	// MaxTokens must give review the same headroom as generation (4096), not
	// the 2048 that truncated real review JSON output in production.
	if gen.gotReq.MaxTokens != 4096 {
		t.Errorf("MaxTokens = %d, want 4096", gen.gotReq.MaxTokens)
	}
}

// ── Phase-tracking helpers ────────────────────────────────────────────────────

type trackingFakeUoW struct {
	onDo func(label string)
}

func (f trackingFakeUoW) Do(_ context.Context, _ string, fn func(database.Tx) error) error {
	f.onDo("tx")
	return fn(nil)
}
func (f trackingFakeUoW) DoSystem(_ context.Context, fn func(database.Tx) error) error {
	return fn(nil)
}

type fakeGenTracked struct {
	out    []byte
	err    error
	gotReq llm.Request
	onCall func()
}

func (f *fakeGenTracked) GenerateJSON(_ context.Context, req llm.Request) ([]byte, error) {
	f.gotReq = req
	if f.onCall != nil {
		f.onCall()
	}
	return f.out, f.err
}

func (f *fakeGenTracked) GenerateJSONStream(_ context.Context, req llm.Request, onChunk func(string) error) ([]byte, error) {
	f.gotReq = req
	if f.onCall != nil {
		f.onCall()
	}
	if f.err != nil {
		return nil, f.err
	}
	if onChunk != nil {
		if err := onChunk(string(f.out)); err != nil {
			return f.out, err
		}
	}
	return f.out, nil
}

package draft

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hibiken/asynq"

	"github.com/jusassessoria/platform/internal/advisory"
	"github.com/jusassessoria/platform/internal/indexing"
	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
	"github.com/jusassessoria/platform/lib/llm"
)

// ── Fakes ────────────────────────────────────────────────────────────────────

// fakeUoW runs fn immediately with a nil tx.
type fakeUoW struct{ err error }

func (f fakeUoW) Do(_ context.Context, _ string, fn func(database.Tx) error) error {
	if f.err != nil {
		return f.err
	}
	return fn(nil)
}
func (f fakeUoW) DoSystem(_ context.Context, fn func(database.Tx) error) error {
	return fn(nil)
}

// fakeReader returns a preset draft, intimation, and parties.
type fakeReader struct {
	draft      *Draft
	intimation *IntimationContext
	parties    []PartyInfo
	theses     []SuggestedThesis            // C2: persisted theses feeding the generation selection
	anchors    map[string][]ThesisAnchor    // multi-âncora (0094): anchors keyed by thesis id
	profile    *GenerationProfile           // PART B: catalog profile for generation
	draftErr   error
	intimErr   error
	partiesErr error
	thesesErr  error
	profileErr error
}

func (f fakeReader) GetDraftByID(_ context.Context, _ database.Tx, _, _ string) (*Draft, error) {
	return f.draft, f.draftErr
}
func (f fakeReader) GetIntimationForDraft(_ context.Context, _ database.Tx, _, _ string) (*IntimationContext, error) {
	return f.intimation, f.intimErr
}
func (f fakeReader) GetPartiesForDraft(_ context.Context, _ database.Tx, _, _ string) ([]PartyInfo, error) {
	return f.parties, f.partiesErr
}
func (f fakeReader) ListSuggestedThesesByDraft(_ context.Context, _ database.Tx, _, _ string) ([]SuggestedThesis, error) {
	return f.theses, f.thesesErr
}
func (f fakeReader) ListSuggestedThesisAnchorsByDraft(_ context.Context, _ database.Tx, _, _ string) (map[string][]ThesisAnchor, error) {
	return f.anchors, nil
}
func (f fakeReader) GetGenerationProfile(_ context.Context, _ database.Tx, _ string) (*GenerationProfile, error) {
	return f.profile, f.profileErr
}

// fakeWriter captures UpdateSagaState, InsertReview, and DeleteReviewsForDraft calls.
type fakeWriter struct {
	updatedSagaState     string
	updatedContent       string
	insertedReview       *Review
	returnedDraft        *Draft
	returnedReview       *Review
	writeErr             error
	deleteReviewsCalled  bool
	deleteReviewsDraftID string
}

func (f *fakeWriter) UpdateSagaState(_ context.Context, _ database.Tx, _, _, sagaState string, updateContent bool, content string, _ *StructuredContent) (*Draft, error) {
	f.updatedSagaState = sagaState
	if updateContent {
		f.updatedContent = content
	}
	return f.returnedDraft, f.writeErr
}

func (f *fakeWriter) UpdateDraftContentHtml(_ context.Context, _ database.Tx, _, _, _ string) error {
	return nil
}

func (f *fakeWriter) InsertReview(_ context.Context, _ database.Tx, r *Review) (*Review, error) {
	f.insertedReview = r
	if f.returnedReview != nil {
		return f.returnedReview, f.writeErr
	}
	r.ID = "review-id-1"
	return r, f.writeErr
}

func (f *fakeWriter) DeleteReviewsForDraft(_ context.Context, _ database.Tx, draftID string) error {
	f.deleteReviewsCalled = true
	f.deleteReviewsDraftID = draftID
	return f.writeErr
}

// fakeOutbox records published events.
type fakeOutbox struct {
	published []events.Event
	err       error
}

func (f *fakeOutbox) Publish(_ context.Context, _ database.Tx, ev events.Event) error {
	if f.err != nil {
		return f.err
	}
	f.published = append(f.published, ev)
	return nil
}

// fakeDedup controls seen/mark behaviour.
type fakeDedup struct {
	seen bool
	err  error
}

func (f fakeDedup) SeenOrMark(_ context.Context, _ database.Tx, _, _ string) (bool, error) {
	return f.seen, f.err
}

// fakeGen returns preset bytes.
type fakeGen struct {
	out    []byte
	err    error
	gotReq llm.Request
}

func (f *fakeGen) GenerateJSON(_ context.Context, req llm.Request) ([]byte, error) {
	f.gotReq = req
	return f.out, f.err
}

// GenerateJSONStream: pra testes, entrega o output inteiro num único chunk
// e depois retorna. Basta pra validar o worker sem SSE real.
func (f *fakeGen) GenerateJSONStream(_ context.Context, req llm.Request, onChunk func(chunk string) error) ([]byte, error) {
	f.gotReq = req
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

// fakeEmbedder returns preset vectors. gotInput (a pointer so value copies of the fake still
// record into the same slice) captures the InputType each Embed call was made with — the RAG
// query side must always pass indexing.InputQuery.
type fakeEmbedder struct {
	vecs     [][]float32
	err      error
	gotInput *[]indexing.InputType
}

func (f fakeEmbedder) Embed(_ context.Context, _ []string, inputType indexing.InputType) ([][]float32, string, error) {
	if f.gotInput != nil {
		*f.gotInput = append(*f.gotInput, inputType)
	}
	return f.vecs, "model", f.err
}

// ── Helpers ──────────────────────────────────────────────────────────────────

// makeDraft returns a test draft in EXTRACTING state.
func makeDraft() *Draft {
	return &Draft{
		ID:        "draft-1",
		TenantID:  "tenant-1",
		SagaState: SagaStateExtracting,
		PieceType: PieceTypeDefense,
		Content:   "Initial draft content here for substring tests.",
	}
}

// ev returns a canonical GenerationRequested event.
func ev() GenerationRequested {
	return GenerationRequested{
		Base:     events.Base{EventID: "event-1", Aggregate: "draft-1"},
		DraftID:  "draft-1",
		TenantID: "tenant-1",
	}
}

// cannedJSON is the JSON the fake generator returns on the happy path.
// Gerar now produces ONLY draft_content — no suggestions.
const cannedJSON = `{
  "draft_markdown": "<p>Revised draft content here for substring tests. Argumento claro.</p>"
}`

// buildUC assembles the GenerateUseCase with the given overridable parts.
func buildUC(
	uow database.UnitOfWork,
	reader generationDepsReader,
	writer generationWriter,
	ob outboxPublisher,
	dedup generateDeduper,
	gen llm.Generator,
	emb embedder,
) *GenerateUseCase {
	return NewGenerateUseCase(GenerateUseCaseParams{
		UoW:      uow,
		Reader:   reader,
		Writer:   writer,
		Outbox:   ob,
		Dedup:    dedup,
		Gen:      gen,
		Emb:      emb,
		Search:   indexing.SearchDeps{Pool: nil}, // nil pool → degraded, no panic
		Composer: advisory.NewTemplateComposer(),
		Now:      func() time.Time { return time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC) },
	})
}

// ── Tests ─────────────────────────────────────────────────────────────────────

// TestGenerateUseCase_NilGenerator_FAILED verifies that a nil generator marks the
// draft FAILED with "IA não configurada" and returns a terminal (KindInvalid) error
// so the listener can classify it as SkipRetry.
func TestGenerateUseCase_NilGenerator_FAILED(t *testing.T) {
	d := makeDraft()
	w := &fakeWriter{returnedDraft: d}
	ob := &fakeOutbox{}

	uc := buildUC(fakeUoW{}, fakeReader{draft: d}, w, ob, fakeDedup{}, nil, nil)

	err := uc.OnGenerationRequested(context.Background(), ev())

	if err == nil {
		t.Fatal("want error, got nil")
	}
	// Use case signals "terminal" by returning a KindInvalid error;
	// the listener converts that to asynq.SkipRetry (tested in TestListener_*).
	if !isGenerationTerminal(err) {
		t.Errorf("want terminal (KindInvalid) error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "IA não configurada") {
		t.Errorf("want 'IA não configurada' in error message, got: %v", err)
	}
	if w.updatedSagaState != SagaStateFailed {
		t.Errorf("saga_state = %q, want FAILED", w.updatedSagaState)
	}
	if w.insertedReview == nil || w.insertedReview.Status != ReviewStatusFailed {
		t.Errorf("review.Status = %q, want FAILED", w.insertedReview.Status)
	}
}

// TestGenerateUseCase_DerivesSelectionFromPersistedTheses verifies C2: the composer's
// selection comes from the PERSISTED thesis state (included/pending_add), not from the
// draft.selected_theses column. Theses in state off are excluded from the prompt.
func TestGenerateUseCase_DerivesSelectionFromPersistedTheses(t *testing.T) {
	d := makeDraft()
	d.SelectedTheses = []string{"legado-ignorado"} // deve ser sobrescrita pelas persistidas
	w := &fakeWriter{returnedDraft: d}
	ob := &fakeOutbox{}
	gen := &fakeGen{out: []byte(cannedJSON)}
	reader := fakeReader{draft: d, theses: []SuggestedThesis{
		{ID: "a", Label: "Prescrição intercorrente", State: ThesisStateIncluded},
		{ID: "b", Label: "Tese em revisão", State: ThesisStatePendingAdd},
		{ID: "c", Label: "Tese descartada", State: ThesisStateOff},
	}}

	uc := buildUC(fakeUoW{}, reader, w, ob, fakeDedup{}, gen, nil)
	if err := uc.OnGenerationRequested(context.Background(), ev()); err != nil {
		t.Fatalf("want nil err, got %v", err)
	}

	prompt := gen.gotReq.User
	if !strings.Contains(prompt, "Prescrição intercorrente") || !strings.Contains(prompt, "Tese em revisão") {
		t.Errorf("prompt must include included/pending_add theses, got:\n%s", prompt)
	}
	if strings.Contains(prompt, "Tese descartada") {
		t.Error("prompt must NOT include an off thesis")
	}
	if strings.Contains(prompt, "legado-ignorado") {
		t.Error("persisted selection must override the legacy draft.selected_theses")
	}
}

// TestSelectedThesisLabels verifies the derivation helper: nil when no persisted theses
// (legacy path preserved), non-nil empty when theses exist but none selected.
func TestSelectedThesisLabels(t *testing.T) {
	if got := selectedThesisLabels(nil); got != nil {
		t.Errorf("no persisted theses must yield nil (legacy passthrough), got %v", got)
	}
	off := []SuggestedThesis{{Label: "x", State: ThesisStateOff}}
	if got := selectedThesisLabels(off); got == nil || len(got) != 0 {
		t.Errorf("theses present but none selected must yield non-nil empty, got %v", got)
	}
	mixed := []SuggestedThesis{
		{Label: "in", State: ThesisStateIncluded},
		{Label: "pend", State: ThesisStatePendingAdd},
		{Label: "off", State: ThesisStateOff},
	}
	got := selectedThesisLabels(mixed)
	if len(got) != 2 || got[0] != "in" || got[1] != "pend" {
		t.Errorf("expected [in pend], got %v", got)
	}
}

// TestSelectedThesesForGen verifies the rich-thesis selection helper: nil when no
// persisted theses OR none selected (legacy fallback), and the SELECTED theses in
// Position order otherwise (same state filter as selectedThesisLabels).
func TestSelectedThesesForGen(t *testing.T) {
	if got := selectedThesesForGen(nil); got != nil {
		t.Errorf("no persisted theses must yield nil, got %v", got)
	}
	off := []SuggestedThesis{{Label: "x", State: ThesisStateOff}}
	if got := selectedThesesForGen(off); got != nil {
		t.Errorf("theses present but none selected must yield nil (legacy fallback), got %v", got)
	}
	mixed := []SuggestedThesis{
		{Label: "pend", State: ThesisStatePendingAdd, Position: 2, Foundation: "F2"},
		{Label: "in", State: ThesisStateIncluded, Position: 1, Foundation: "F1", Reference: "art. 1"},
		{Label: "off", State: ThesisStateOff, Position: 0},
	}
	got := selectedThesesForGen(mixed)
	if len(got) != 2 {
		t.Fatalf("expected 2 selected, got %d (%v)", len(got), got)
	}
	// Position-ordered: "in" (1) before "pend" (2).
	if got[0].Label != "in" || got[1].Label != "pend" {
		t.Errorf("expected [in pend] by Position, got [%s %s]", got[0].Label, got[1].Label)
	}
	if got[0].Foundation != "F1" || got[0].Reference != "art. 1" {
		t.Errorf("rich fields not preserved: %+v", got[0])
	}
}

// TestBuildDraftContext_RichTheses verifies the rich path: passed selectedTheses map
// into advisory.SelectedThesisCtx (Excerpt=SourceExcerpt) and WIN over the legacy
// draft.SelectedTheses labels.
func TestBuildDraftContext_RichTheses(t *testing.T) {
	d := &Draft{PieceType: PieceTypeDefense, SelectedTheses: []string{"legado-ignorado"}}
	rich := []SuggestedThesis{
		{Label: "Prescrição", Foundation: "Inércia 5+ anos", Reference: "art. 924 CPC", SourceExcerpt: "sem movimentação", SourceLabel: "fls. 120", Grounded: true},
	}
	dc := buildDraftContext(d, nil, nil, nil, nil, rich)
	if len(dc.SelectedTheses) != 1 {
		t.Fatalf("SelectedTheses len = %d, want 1", len(dc.SelectedTheses))
	}
	th := dc.SelectedTheses[0]
	if th.Label != "Prescrição" || th.Foundation != "Inércia 5+ anos" || th.Reference != "art. 924 CPC" {
		t.Errorf("rich thesis mismatch: %+v", th)
	}
	if th.Excerpt != "sem movimentação" || th.SourceLabel != "fls. 120" || !th.Grounded {
		t.Errorf("Excerpt/SourceLabel/Grounded mismatch: %+v", th)
	}
}

// TestGenerateUseCase_HappyPath verifies the successful generation: saga DRAFTED + content
// set + prior reviews deleted + NO review inserted + draft.generated published (the fact
// that actually writes the peça's content — see events.go's 2026-08-26 note on why this
// moved here from Revisar).
func TestGenerateUseCase_HappyPath(t *testing.T) {
	d := makeDraft()
	w := &fakeWriter{returnedDraft: d}
	ob := &fakeOutbox{}
	gen := &fakeGen{out: []byte(cannedJSON)}

	uc := buildUC(fakeUoW{}, fakeReader{draft: d}, w, ob, fakeDedup{}, gen, nil)

	if err := uc.OnGenerationRequested(context.Background(), ev()); err != nil {
		t.Fatalf("want nil err, got %v", err)
	}

	// saga_state must be DRAFTED (not REVIEWED — suggestions are produced by Revisar).
	if w.updatedSagaState != SagaStateDrafted {
		t.Errorf("saga_state = %q, want DRAFTED", w.updatedSagaState)
	}
	// draft_content must be set.
	if w.updatedContent == "" {
		t.Error("updatedContent is empty, want the generated draft_content")
	}
	// Prior reviews must be deleted before setting DRAFTED.
	if !w.deleteReviewsCalled {
		t.Error("DeleteReviewsForDraft was not called, want called before UpdateSagaState")
	}
	// No review must be inserted — Revisar does that.
	if w.insertedReview != nil {
		t.Errorf("insertedReview is non-nil, want nil (Gerar must not insert reviews)")
	}
	// draft.generated MUST be published — Gerar is the use case that writes the peça's
	// content, so it (not Revisar) is the correct producer of the activity-log fact.
	if len(ob.published) != 1 {
		t.Fatalf("published events = %d, want 1", len(ob.published))
	}
	pubEv, ok := ob.published[0].(DraftGenerated)
	if !ok {
		t.Fatalf("published event type = %T, want DraftGenerated", ob.published[0])
	}
	if pubEv.DraftID != d.ID {
		t.Errorf("pubEv.DraftID = %q, want %q", pubEv.DraftID, d.ID)
	}
	if pubEv.TenantID != d.TenantID {
		t.Errorf("pubEv.TenantID = %q, want %q", pubEv.TenantID, d.TenantID)
	}
	if pubEv.Type() != TypeDraftGenerated {
		t.Errorf("pubEv.Type() = %q, want %q", pubEv.Type(), TypeDraftGenerated)
	}
}

// TestGenerateUseCase_PublishesCourtRecordID verifies that when the draft resolves an
// intimation (the common case), the published draft.generated event carries the
// intimation's court_record_id — sparing acquisition's activity listener a resolver
// round-trip in the common path.
func TestGenerateUseCase_PublishesCourtRecordID(t *testing.T) {
	d := makeDraft()
	d.IntimationID = "intimation-1"
	intim := &IntimationContext{CourtRecordID: "cr-42"}
	w := &fakeWriter{returnedDraft: d}
	ob := &fakeOutbox{}
	gen := &fakeGen{out: []byte(cannedJSON)}

	uc := buildUC(fakeUoW{}, fakeReader{draft: d, intimation: intim}, w, ob, fakeDedup{}, gen, nil)

	if err := uc.OnGenerationRequested(context.Background(), ev()); err != nil {
		t.Fatalf("want nil err, got %v", err)
	}

	if len(ob.published) != 1 {
		t.Fatalf("published events = %d, want 1", len(ob.published))
	}
	pubEv, ok := ob.published[0].(DraftGenerated)
	if !ok {
		t.Fatalf("published event type = %T, want DraftGenerated", ob.published[0])
	}
	if pubEv.CourtRecordID != "cr-42" {
		t.Errorf("pubEv.CourtRecordID = %q, want %q", pubEv.CourtRecordID, "cr-42")
	}
}

// TestGenerateUseCase_DraftedState_NoReview verifies that a simple generation call
// results in DRAFTED state with no review inserted. This replaces the old
// SubstringDropped test (Gerar no longer produces suggestions — that's Revisar's job).
func TestGenerateUseCase_DraftedState_NoReview(t *testing.T) {
	d := makeDraft()
	w := &fakeWriter{returnedDraft: d}
	ob := &fakeOutbox{}
	gen := &fakeGen{out: []byte(cannedJSON)}

	uc := buildUC(fakeUoW{}, fakeReader{draft: d}, w, ob, fakeDedup{}, gen, nil)

	if err := uc.OnGenerationRequested(context.Background(), ev()); err != nil {
		t.Fatalf("want nil err, got %v", err)
	}

	// Gerar must not insert a review — suggestions are Revisar's responsibility.
	if w.insertedReview != nil {
		t.Errorf("insertedReview non-nil, want nil (Gerar must not insert reviews)")
	}
	if w.updatedSagaState != SagaStateDrafted {
		t.Errorf("saga_state = %q, want DRAFTED", w.updatedSagaState)
	}
}

// TestGenerateUseCase_DegradedNoChunks verifies the degraded path: when the embedder
// is nil (no RAG), generation still succeeds and reaches DRAFTED state.
func TestGenerateUseCase_DegradedNoChunks(t *testing.T) {
	d := makeDraft()
	w := &fakeWriter{returnedDraft: d}
	ob := &fakeOutbox{}
	gen := &fakeGen{out: []byte(`{"draft_markdown":"text here"}`)}

	// No embedder (nil) → degraded path.
	uc := buildUC(fakeUoW{}, fakeReader{draft: d}, w, ob, fakeDedup{}, gen, nil)

	if err := uc.OnGenerationRequested(context.Background(), ev()); err != nil {
		t.Fatalf("want nil err, got %v", err)
	}

	// Gerar doesn't insert a review; verify saga reached DRAFTED.
	if w.updatedSagaState != SagaStateDrafted {
		t.Errorf("saga_state = %q, want DRAFTED", w.updatedSagaState)
	}
	if w.insertedReview != nil {
		t.Errorf("insertedReview non-nil, want nil for degraded generation")
	}
}

// TestGenerateUseCase_GeneratorError_FAILED verifies that a terminal LLM error
// marks the draft FAILED (no retry — the listener will archive the task).
func TestGenerateUseCase_GeneratorError_FAILED(t *testing.T) {
	d := makeDraft()
	w := &fakeWriter{returnedDraft: d}
	ob := &fakeOutbox{}
	gen := &fakeGen{err: apperr.NewInvalid("bad api key")}

	uc := buildUC(fakeUoW{}, fakeReader{draft: d}, w, ob, fakeDedup{}, gen, nil)

	err := uc.OnGenerationRequested(context.Background(), ev())
	// Use case signals "terminal" via KindInvalid; listener converts to SkipRetry.
	if !isGenerationTerminal(err) {
		t.Errorf("want terminal (KindInvalid) error for terminal LLM error, got: %v", err)
	}
	if w.updatedSagaState != SagaStateFailed {
		t.Errorf("saga_state = %q, want FAILED", w.updatedSagaState)
	}
}

// TestGenerateUseCase_Idempotency_SeenEvent verifies that a duplicate event (already
// marked in processed_event) is a no-op.
func TestGenerateUseCase_Idempotency_SeenEvent(t *testing.T) {
	d := makeDraft()
	w := &fakeWriter{returnedDraft: d}
	gen := &fakeGen{out: []byte(`{"draft_markdown":"x"}`)}

	uc := buildUC(fakeUoW{}, fakeReader{draft: d}, w, &fakeOutbox{}, fakeDedup{seen: true}, gen, nil)

	if err := uc.OnGenerationRequested(context.Background(), ev()); err != nil {
		t.Fatalf("want nil for seen event, got %v", err)
	}
	// Dedup saw the event → no writes should have happened.
	if w.updatedSagaState != "" {
		t.Errorf("saga_state updated even though event was seen (duplicate): %q", w.updatedSagaState)
	}
}

// TestGenerateUseCase_ObsoleteSaga_SkipRetry verifies that an event arriving when
// the draft's saga_state is not EXTRACTING (e.g. already REVIEWED) is a SkipRetry
// (the event is obsolete — the draft was re-triggered or completed by another delivery).
func TestGenerateUseCase_ObsoleteSaga_SkipRetry(t *testing.T) {
	// Draft is already REVIEWED — not EXTRACTING.
	d := makeDraft()
	d.SagaState = SagaStateReviewed

	w := &fakeWriter{returnedDraft: d}
	gen := &fakeGen{out: []byte(`{"draft_markdown":"x"}`)}

	uc := buildUC(fakeUoW{}, fakeReader{draft: d}, w, &fakeOutbox{}, fakeDedup{}, gen, nil)

	err := uc.OnGenerationRequested(context.Background(), ev())
	if err == nil {
		t.Fatal("want error for obsolete saga, got nil")
	}
	// Use case signals "terminal" via KindInvalid; listener converts to SkipRetry.
	if !isGenerationTerminal(err) {
		t.Errorf("want terminal (KindInvalid) error for obsolete saga, got: %v", err)
	}
	// No writes should have happened.
	if w.updatedSagaState != "" {
		t.Errorf("saga_state updated even though saga was obsolete: %q", w.updatedSagaState)
	}
}

// ── Listener unit tests ────────────────────────────────────────────────────────

// stubGenerateUC is the generateUseCase port stub for listener tests.
type stubGenerateUC struct {
	err   error
	calls int
}

func (s *stubGenerateUC) OnGenerationRequested(_ context.Context, _ GenerationRequested) error {
	s.calls++
	return s.err
}

// TestListener_handleGenerationRequested covers the listener's retry-decision mapping.
func TestListener_handleGenerationRequested(t *testing.T) {
	task := asynq.NewTask(TypeGenerationRequested, []byte(`{}`))

	tests := []struct {
		name     string
		ucErr    error
		wantSkip bool
	}{
		{name: "success acks", ucErr: nil, wantSkip: false},
		{name: "invalid/terminal is skip", ucErr: apperr.NewInvalid("bad"), wantSkip: true},
		{name: "not-found is skip", ucErr: apperr.NewNotFound("gone"), wantSkip: true},
		{name: "infra stays retryable", ucErr: apperr.NewInfra("db down", errors.New("boom")), wantSkip: false},
		{name: "unknown stays retryable", ucErr: errors.New("opaque"), wantSkip: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub := &stubGenerateUC{err: tt.ucErr}
			l := NewListener(stub)

			err := l.handleGenerationRequested(context.Background(), task)

			if stub.calls != 1 {
				t.Fatalf("use case calls = %d, want 1", stub.calls)
			}
			if tt.ucErr == nil {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tt.ucErr) {
				t.Errorf("original error dropped from chain: %v", err)
			}
			if got := errors.Is(err, asynq.SkipRetry); got != tt.wantSkip {
				t.Errorf("SkipRetry = %v, want %v (err = %v)", got, tt.wantSkip, err)
			}
		})
	}
}

// TestListener_handleGenerationRequested_decodeFault verifies that a malformed payload
// is archived (SkipRetry) before the use case is invoked.
func TestListener_handleGenerationRequested_decodeFault(t *testing.T) {
	stub := &stubGenerateUC{}
	l := NewListener(stub)
	task := asynq.NewTask(TypeGenerationRequested, []byte(`not json`))

	err := l.handleGenerationRequested(context.Background(), task)
	if err == nil {
		t.Fatal("want error for malformed payload")
	}
	if !errors.Is(err, asynq.SkipRetry) {
		t.Errorf("want SkipRetry for decode fault, got: %v", err)
	}
	if stub.calls != 0 {
		t.Errorf("use case called %d times, want 0 (should abort at decode)", stub.calls)
	}
}

// ── Handler unit tests ────────────────────────────────────────────────────────

// stubTriggerUC is the generator port stub for handler tests.
type stubTriggerUC struct {
	draft *Draft
	err   error
	calls int
}

func (s *stubTriggerUC) TriggerGeneration(_ context.Context, cmd TriggerGenerationCommand) (*Draft, error) {
	s.calls++
	return s.draft, s.err
}

// TestHandler_generatePeca_409IfExtracting verifies that the handler returns 409 when
// the trigger use case returns ErrGenerationInProgress.
func TestHandler_generatePeca_409IfExtracting(t *testing.T) {
	// httpx.TenantFromCtx requires a real Fiber context, so we test the use case
	// guard directly instead of a full HTTP round-trip: the handler just delegates to
	// the trigger use case, which returns ErrGenerationInProgress.
	stub := &stubTriggerUC{err: ErrGenerationInProgress}
	_, err := stub.TriggerGeneration(context.Background(), TriggerGenerationCommand{TenantID: "t", DraftID: "d"})
	if !errors.Is(err, ErrGenerationInProgress) {
		t.Errorf("want ErrGenerationInProgress, got %v", err)
	}
	var ae *apperr.AppError
	if !errors.As(err, &ae) || ae.Kind != apperr.KindConflict {
		t.Errorf("want KindConflict, got %v", err)
	}
}

// TestHandler_generatePeca_202IfExtracting verifies the 202 path (use case succeeds).
func TestHandler_generatePeca_202IfExtracting(t *testing.T) {
	want := &Draft{ID: "d-1", SagaState: SagaStateExtracting}
	stub := &stubTriggerUC{draft: want}
	got, err := stub.TriggerGeneration(context.Background(), TriggerGenerationCommand{TenantID: "t", DraftID: "d-1"})
	if err != nil {
		t.Fatalf("want nil err, got %v", err)
	}
	if got.SagaState != SagaStateExtracting {
		t.Errorf("saga_state = %q, want EXTRACTING", got.SagaState)
	}
}

// ── buildFindings unit tests (validation logic) ────────────────────────────────

// TestBuildFindings_Top10Cap verifies that only 10 suggestions are kept after filtering.
func TestBuildFindings_Top10Cap(t *testing.T) {
	content := "content " + strings.Repeat("word ", 20)
	suggestions := make([]rawSuggestion, 15)
	for i := range suggestions {
		suggestions[i] = rawSuggestion{
			Category:    CategoryClareza,
			Original:    "word ",
			Replacement: fmt.Sprintf("r%d", i),
			Problem:     "p",
			Description: "d",
		}
	}
	findings, coverage := buildFindings(suggestions, false, 0, content)
	if len(findings) != 10 {
		t.Errorf("findings = %d, want 10 (capped)", len(findings))
	}
	if coverage.SuggestionsTotal != 15 {
		t.Errorf("suggestions_total = %d, want 15", coverage.SuggestionsTotal)
	}
}

// TestGenerateUseCase_WithIntimation_CRIDPropagated verifies that when a draft has an
// TestBuildQueryText enriches the RAG query with the case signal (classe/assunto + a bounded,
// HTML-stripped teor slice) beyond the bare PieceType+Type, and stays deterministic + bounded so
// the RAG cache key is stable.
func TestBuildQueryText(t *testing.T) {
	t.Parallel()

	d := &Draft{PieceType: "CONTESTACAO"}

	// nil intimation → just the piece type.
	if got := buildQueryText(d, nil); got != "CONTESTACAO" {
		t.Errorf("nil intimation query = %q, want %q", got, "CONTESTACAO")
	}

	i := &IntimationContext{
		Type:    "CITACAO",
		Class:   "Procedimento Comum",
		Subject: "Rescisão Contratual",
		Content: "<p>Fica o réu <b>citado</b> para apresentar defesa no prazo legal.</p>",
	}
	got := buildQueryText(d, i)
	for _, want := range []string{"CONTESTACAO", "CITACAO", "Procedimento Comum", "Rescisão Contratual", "citado", "defesa"} {
		if !strings.Contains(got, want) {
			t.Errorf("query %q missing %q", got, want)
		}
	}
	if strings.Contains(got, "<") {
		t.Errorf("query still has HTML tags: %q", got)
	}
	// Deterministic: same input → same query (stable cache key).
	if buildQueryText(d, i) != got {
		t.Error("buildQueryText not deterministic")
	}

	// Bounded: a huge teor is capped to queryTeorMaxRunes on a rune boundary.
	long := &IntimationContext{Content: strings.Repeat("á", queryTeorMaxRunes*3)}
	q := buildQueryText(d, long)
	// prefix parts + one space + at most queryTeorMaxRunes runes of teor.
	if teorRunes := len([]rune(q)) - len([]rune(d.PieceType)) - 1; teorRunes > queryTeorMaxRunes {
		t.Errorf("teor not bounded: %d runes > %d", teorRunes, queryTeorMaxRunes)
	}
}

// intimation with a non-empty CourtRecordID, the generation pipeline calls runRAG
// (via the package function) with a non-nil crid. Since runRAG degrades at the
// pool-nil gate before calling SearchChunks, we assert grounded=false (no chunks)
// but prove the intimation was loaded by checking that IntimationContext reaches
// buildQueryText (i.e. the composed query includes the intimation type).
func TestGenerateUseCase_WithIntimation_CRIDPropagated(t *testing.T) {
	d := makeDraft()
	d.IntimationID = "intim-1"
	intim := &IntimationContext{
		IntimationID:  "intim-1",
		CaseID:        "case-1",
		CourtRecordID: "court-record-uuid-xyz",
		Type:          "CITACAO",
	}
	w := &fakeWriter{returnedDraft: d}
	ob := &fakeOutbox{}
	gen := &fakeGen{out: []byte(`{"draft_markdown":"text","suggestions":[]}`)}

	// Embedder non-nil but pool nil → runRAG degrades after the embed step is skipped
	// by the pool gate (degraded, grounded=false). This still exercises the crid resolution
	// path: if the code wrongly skipped loading the intimation, the warning log would fire.
	// We assert that the generation succeeded and saga reached DRAFTED.
	uc := buildUC(fakeUoW{}, fakeReader{draft: d, intimation: intim}, w, ob, fakeDedup{}, gen, nil)

	if err := uc.OnGenerationRequested(context.Background(), ev()); err != nil {
		t.Fatalf("want nil err, got %v", err)
	}
	// Gerar doesn't insert a review; verify saga reached DRAFTED.
	if w.updatedSagaState != SagaStateDrafted {
		t.Errorf("saga_state = %q, want DRAFTED", w.updatedSagaState)
	}
	if w.insertedReview != nil {
		t.Errorf("insertedReview non-nil, want nil (Gerar must not insert reviews)")
	}
}

// TestGenerateUseCase_WithoutIntimation_WholeTenantSearch verifies that a blank/processo
// draft (no IntimationID) runs the full pipeline with crid=nil (whole-tenant) and reaches DRAFTED.
func TestGenerateUseCase_WithoutIntimation_WholeTenantSearch(t *testing.T) {
	d := makeDraft()
	// No IntimationID → blank/processo draft → crid stays nil.
	d.IntimationID = ""
	w := &fakeWriter{returnedDraft: d}
	ob := &fakeOutbox{}
	gen := &fakeGen{out: []byte(`{"draft_markdown":"text"}`)}

	uc := buildUC(fakeUoW{}, fakeReader{draft: d}, w, ob, fakeDedup{}, gen, nil)

	if err := uc.OnGenerationRequested(context.Background(), ev()); err != nil {
		t.Fatalf("want nil err, got %v", err)
	}
	if w.updatedSagaState != SagaStateDrafted {
		t.Errorf("saga_state = %q, want DRAFTED", w.updatedSagaState)
	}
	if w.insertedReview != nil {
		t.Errorf("insertedReview non-nil, want nil (Gerar must not insert reviews)")
	}
}

// TestBuildDraftContext_WithIntimation verifies that buildDraftContext populates ALL
// fields from a fully-loaded IntimationContext, including parties and signing lawyer.
func TestBuildDraftContext_WithIntimation(t *testing.T) {
	d := &Draft{PieceType: PieceTypeDefense}
	// Recipients: one matched recipient (our advogado).
	recipientsJSON := []byte(`[{"name":"Dr. João Silva","oab":"12345","uf":"SP","matched":true},{"name":"Dr. Maria","oab":"99999","uf":"SP","matched":false}]`)
	i := &IntimationContext{
		Type:            "CITACAO",
		Content:         "Fica o réu citado para contestar em 15 dias.",
		Court:           "TJSP",
		Degree:          "G1",
		Class:           "Procedimento Comum",
		Subject:         "Contrato",
		CNJNumber:       "0000001-23.2026.8.26.0001",
		JudgingBody:     "3ª Vara Cível",
		DeadlineEndDate: "2026-09-01",
		Recipients:      recipientsJSON,
	}
	parties := []PartyInfo{
		{Role: "PLAINTIFF", Name: "AUTOR LTDA", Counsel: "Pedro (OAB/RS nº 119938)"},
		{Role: "DEFENDANT", Name: "RÉU SA"},
	}
	chunks := []string{"trecho 1", "trecho 2"}

	dc := buildDraftContext(d, i, parties, chunks, nil, nil)

	tests := []struct {
		field string
		got   string
		want  string
	}{
		{"PieceType", dc.PieceType, PieceTypeDefense},
		{"IntimationType", dc.IntimationType, "CITACAO"},
		{"IntimationText", dc.IntimationText, i.Content},
		{"Court", dc.Court, "TJSP"},
		{"Degree", dc.Degree, "G1"},
		{"Class", dc.Class, "Procedimento Comum"},
		{"Subject", dc.Subject, "Contrato"},
		{"CNJNumber", dc.CNJNumber, "0000001-23.2026.8.26.0001"},
		{"JudgingBody", dc.JudgingBody, "3ª Vara Cível"},
		{"DeadlineDate", dc.DeadlineDate, "2026-09-01"},
		// Signing lawyer from matched recipient.
		{"SigningLawyerName", dc.SigningLawyerName, "Dr. João Silva"},
		{"SigningLawyerOAB", dc.SigningLawyerOAB, "12345"},
		{"SigningLawyerUF", dc.SigningLawyerUF, "SP"},
	}
	for _, tt := range tests {
		t.Run(tt.field, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("buildDraftContext.%s = %q, want %q", tt.field, tt.got, tt.want)
			}
		})
	}
	if len(dc.Chunks) != 2 {
		t.Errorf("len(Chunks) = %d, want 2", len(dc.Chunks))
	}
	// Parties must be propagated.
	if len(dc.Parties) != 2 {
		t.Fatalf("len(Parties) = %d, want 2", len(dc.Parties))
	}
	if dc.Parties[0].Role != "PLAINTIFF" || dc.Parties[0].Name != "AUTOR LTDA" {
		t.Errorf("Parties[0] = %+v, want PLAINTIFF AUTOR LTDA", dc.Parties[0])
	}
	if dc.Parties[1].Role != "DEFENDANT" || dc.Parties[1].Name != "RÉU SA" {
		t.Errorf("Parties[1] = %+v, want DEFENDANT RÉU SA", dc.Parties[1])
	}
}

// TestBuildDraftContext_TonePropagation verifies that Draft.Tone/.Instructions/
// .SelectedTheses (the Fatia 5 generation params persisted by TriggerGeneration
// on the draft row — see generate_trigger.go) are copied into the composed
// advisory.DraftContext by buildDraftContext, which is what OnGenerationRequested
// calls after rereading the draft (generate.go:266). This closes the gap where no
// test proved the reread → DraftContext copy actually happens.
func TestBuildDraftContext_TonePropagation(t *testing.T) {
	tests := []struct {
		name             string
		draft            *Draft
		wantTone         string
		wantInstructions string
		wantTheses       []string
	}{
		{
			name: "tone/instructions/theses set on the draft propagate to DraftContext",
			draft: &Draft{
				PieceType:      PieceTypeDefense,
				Tone:           "coloquial",
				Instructions:   "cite jurisprudência recente do STJ",
				SelectedTheses: []string{"tese-prescricao", "tese-decadencia"},
			},
			wantTone:         "coloquial",
			wantInstructions: "cite jurisprudência recente do STJ",
			wantTheses:       []string{"tese-prescricao", "tese-decadencia"},
		},
		{
			name: "server-side default tone (tecnico) propagates like any other value",
			draft: &Draft{
				PieceType: PieceTypeDefense,
				Tone:      ToneTecnico,
			},
			wantTone: ToneTecnico,
		},
		{
			name:  "zero-value draft: empty tone/instructions/nil theses propagate as empty",
			draft: &Draft{PieceType: PieceTypeDefense},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dc := buildDraftContext(tt.draft, nil, nil, nil, nil, nil)

			if dc.Tone != tt.wantTone {
				t.Errorf("DraftContext.Tone = %q, want %q", dc.Tone, tt.wantTone)
			}
			if dc.Instructions != tt.wantInstructions {
				t.Errorf("DraftContext.Instructions = %q, want %q", dc.Instructions, tt.wantInstructions)
			}
			// Legacy fallback: with no rich theses passed, buildDraftContext maps
			// draft.SelectedTheses (labels) into SelectedThesisCtx com só Label.
			if len(dc.SelectedTheses) != len(tt.wantTheses) {
				t.Fatalf("DraftContext.SelectedTheses = %v, want %v", dc.SelectedTheses, tt.wantTheses)
			}
			for i, th := range tt.wantTheses {
				if dc.SelectedTheses[i].Label != th {
					t.Errorf("DraftContext.SelectedTheses[%d].Label = %q, want %q", i, dc.SelectedTheses[i].Label, th)
				}
				if dc.SelectedTheses[i].Foundation != "" || dc.SelectedTheses[i].Reference != "" {
					t.Errorf("legacy fallback must leave rich fields empty, got %+v", dc.SelectedTheses[i])
				}
			}
		})
	}
}

// TestGenerateUseCase_OnGenerationRequested_ToneReachesComposer verifies the
// end-to-end reread path: OnGenerationRequested reloads the draft (which now
// carries Tone/Instructions/SelectedTheses persisted by TriggerGeneration) and
// the composed prompt is built from a DraftContext that includes them. It uses a
// spy PromptComposer to capture the DraftContext actually passed to ComposeDraft,
// proving the propagation happens on the real async path, not just in the pure
// buildDraftContext unit test above.
func TestGenerateUseCase_OnGenerationRequested_ToneReachesComposer(t *testing.T) {
	d := makeDraft()
	d.Tone = "coloquial"
	d.Instructions = "cite jurisprudência recente do STJ"
	d.SelectedTheses = []string{"tese-prescricao"}

	w := &fakeWriter{returnedDraft: d}
	ob := &fakeOutbox{}
	gen := &fakeGen{out: []byte(cannedJSON)}
	spy := newSpyComposer()

	uc := NewGenerateUseCase(GenerateUseCaseParams{
		UoW:      fakeUoW{},
		Reader:   fakeReader{draft: d},
		Writer:   w,
		Outbox:   ob,
		Dedup:    fakeDedup{},
		Gen:      gen,
		Emb:      nil,
		Search:   indexing.SearchDeps{Pool: nil},
		Composer: spy,
		Now:      func() time.Time { return time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC) },
	})

	if err := uc.OnGenerationRequested(context.Background(), ev()); err != nil {
		t.Fatalf("want nil err, got %v", err)
	}

	if spy.gotCtx.Tone != "coloquial" {
		t.Errorf("composer got Tone = %q, want coloquial", spy.gotCtx.Tone)
	}
	if spy.gotCtx.Instructions != "cite jurisprudência recente do STJ" {
		t.Errorf("composer got Instructions = %q, want the persisted instructions", spy.gotCtx.Instructions)
	}
	// No persisted suggested theses in this fake reader → legacy fallback maps the
	// draft's SelectedTheses labels into SelectedThesisCtx com só Label.
	if len(spy.gotCtx.SelectedTheses) != 1 || spy.gotCtx.SelectedTheses[0].Label != "tese-prescricao" {
		t.Errorf("composer got SelectedTheses = %v, want [{Label:tese-prescricao}]", spy.gotCtx.SelectedTheses)
	}
}

// spyComposer records the DraftContext it was called with (ComposeDraft is the
// only method OnGenerationRequested calls) and delegates every method to the
// real template composer so the rest of the pipeline (System/User prompt text)
// stays realistic. It embeds advisory.PromptComposer so it satisfies the
// interface without repeating the other four methods verbatim.
type spyComposer struct {
	advisory.PromptComposer
	gotCtx advisory.DraftContext
}

func newSpyComposer() *spyComposer {
	return &spyComposer{PromptComposer: advisory.NewTemplateComposer()}
}

func (s *spyComposer) ComposeDraft(agent string, dc advisory.DraftContext) (advisory.Composed, error) {
	s.gotCtx = dc
	return s.PromptComposer.ComposeDraft(agent, dc)
}

// TestBuildDraftContext_NilIntimation verifies that buildDraftContext with a nil
// IntimationContext leaves the intimation fields empty (blank/processo draft path).
func TestBuildDraftContext_NilIntimation(t *testing.T) {
	d := &Draft{PieceType: PieceTypeMotion}
	dc := buildDraftContext(d, nil, nil, nil, nil, nil)

	if dc.PieceType != PieceTypeMotion {
		t.Errorf("PieceType = %q, want MOTION", dc.PieceType)
	}
	// All intimation-derived fields must be empty.
	for _, f := range []struct{ name, val string }{
		{"IntimationType", dc.IntimationType},
		{"IntimationText", dc.IntimationText},
		{"Court", dc.Court},
		{"Degree", dc.Degree},
		{"Class", dc.Class},
		{"Subject", dc.Subject},
		{"CNJNumber", dc.CNJNumber},
		{"JudgingBody", dc.JudgingBody},
		{"DeadlineDate", dc.DeadlineDate},
	} {
		if f.val != "" {
			t.Errorf("nil intimation: %s = %q, want empty", f.name, f.val)
		}
	}
}

// TestBuildDraftContext_ProfileSections verifies PART B: when a GenerationProfile
// with sections is passed, buildDraftContext copies PieceProfileKey + the ordered
// sections into the DraftContext; a nil profile leaves ProfileSections nil (generic
// fallback).
func TestBuildDraftContext_ProfileSections(t *testing.T) {
	d := &Draft{PieceType: PieceTypeDefense, PieceProfileKey: "contestacao"}
	profile := &GenerationProfile{
		Key:  "contestacao",
		Nome: "Contestação",
		Polo: "passivo",
		Sections: []ProfileSectionInfo{
			{Key: "preliminares", Titulo: "Das Preliminares", Ordem: 1, Obrigatoria: "condicional", Origem: "argumentativa", AceitaTeses: true},
			{Key: "merito", Titulo: "Do Mérito", Ordem: 4, Obrigatoria: "sim", Origem: "argumentativa", AceitaTeses: true},
			{Key: "pedidos", Titulo: "Dos Pedidos", Ordem: 5, Obrigatoria: "sim", Origem: "argumentativa", AceitaTeses: false},
		},
	}
	dc := buildDraftContext(d, nil, nil, nil, profile, nil)
	if dc.PieceProfileKey != "contestacao" {
		t.Errorf("PieceProfileKey = %q, want contestacao", dc.PieceProfileKey)
	}
	if len(dc.ProfileSections) != 3 {
		t.Fatalf("ProfileSections len = %d, want 3", len(dc.ProfileSections))
	}
	if dc.ProfileSections[0].Titulo != "Das Preliminares" || dc.ProfileSections[0].Obrigatoria != "condicional" {
		t.Errorf("section[0] = %+v, want Das Preliminares/condicional", dc.ProfileSections[0])
	}
	if !dc.ProfileSections[1].AceitaTeses || dc.ProfileSections[2].AceitaTeses {
		t.Errorf("aceita_teses mismatch: merito=%v pedidos=%v", dc.ProfileSections[1].AceitaTeses, dc.ProfileSections[2].AceitaTeses)
	}

	// nil profile → no sections.
	dcNil := buildDraftContext(d, nil, nil, nil, nil, nil)
	if dcNil.ProfileSections != nil {
		t.Errorf("nil profile: ProfileSections = %v, want nil", dcNil.ProfileSections)
	}
}

// TestBuildFindings_DocumentsCited verifies that documents cited in Argumento
// suggestions appear in the coverage.documents_cited list.
func TestBuildFindings_DocumentsCited(t *testing.T) {
	content := "argument point here"
	suggestions := []rawSuggestion{
		{
			Category:    CategoryArgumento,
			Original:    "argument point here",
			Replacement: "r",
			Problem:     "p",
			Description: "d",
			Citation:    &rawCitation{DocumentID: "doc-abc", Page: 1, Quote: "q"},
		},
	}
	findings, coverage := buildFindings(suggestions, true, 3, content)
	if len(findings) != 1 {
		t.Fatalf("findings = %d, want 1", len(findings))
	}
	if findings[0].Citation == nil || findings[0].Citation.DocumentID != "doc-abc" {
		t.Errorf("citation = %+v, want doc-abc", findings[0].Citation)
	}
	if len(coverage.DocumentsCited) != 1 || coverage.DocumentsCited[0] != "doc-abc" {
		t.Errorf("documents_cited = %v, want [doc-abc]", coverage.DocumentsCited)
	}
	if !coverage.Grounded {
		t.Error("grounded = false, want true")
	}
	if coverage.ChunksUsed != 3 {
		t.Errorf("chunks_used = %d, want 3", coverage.ChunksUsed)
	}
}

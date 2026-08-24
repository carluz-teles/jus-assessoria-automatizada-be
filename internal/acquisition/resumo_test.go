package acquisition

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jusassessoria/platform/internal/advisory"
	"github.com/jusassessoria/platform/internal/indexing"
	"github.com/jusassessoria/platform/lib/llm"
)

// fakeResumoReader is the resumoReader port fake. It returns a canned context or the
// configured error, recording the ids it was called with.
type fakeResumoReader struct {
	ctx ProcessoResumoCtx
	err error
}

func (f fakeResumoReader) GetResumoContext(_ context.Context, _, _ string) (ProcessoResumoCtx, error) {
	return f.ctx, f.err
}

// fakeResumoGen is the llm.Generator fake for the resume path.
type fakeResumoGen struct {
	out    []byte
	err    error
	calls  int
	gotReq llm.Request
}

func (f *fakeResumoGen) GenerateJSON(_ context.Context, req llm.Request) ([]byte, error) {
	f.calls++
	f.gotReq = req
	return f.out, f.err
}

func (f *fakeResumoGen) GenerateJSONStream(_ context.Context, req llm.Request, onChunk func(string) error) ([]byte, error) {
	f.calls++
	f.gotReq = req
	if f.err != nil {
		return nil, f.err
	}
	if onChunk != nil {
		_ = onChunk(string(f.out))
	}
	return f.out, nil
}

// fakeResumoStore is the resumoStore fake. It records the persisted JSON and can be
// told to fail — the use case must NOT propagate a store fault (best-effort).
type fakeResumoStore struct {
	calls       int
	gotTenantID string
	gotRecordID string
	gotJSON     []byte
	err         error
}

func (f *fakeResumoStore) SaveResumo(_ context.Context, tenantID, courtRecordID string, rawJSON []byte) error {
	f.calls++
	f.gotTenantID = tenantID
	f.gotRecordID = courtRecordID
	f.gotJSON = rawJSON
	return f.err
}

func baseCtx() ProcessoResumoCtx {
	t := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	return ProcessoResumoCtx{
		ID:        "rec-1",
		CNJNumber: "0000001-23.2026.8.26.0001",
		Court:     "TJSP",
		Degree:    "G1",
		Class:     "PROCEDIMENTO_COMUM",
		Subject:   "Contrato",
		Lifecycle: "ACTIVE",
		FiledAt:   &t,
		RecentMovements: []RawDocketEntry{
			{OccurredAt: t, Text: "Partes intimadas para manifestação"},
			{OccurredAt: t.Add(-24 * time.Hour), Text: "Conclusos para decisão"},
		},
		ActiveIntimations: []RawIntimation{
			{Type: "INTIMACAO", Teor: "Manifeste-se sobre a contestação", DeadlineDays: 5},
		},
		OpenDeadlines: []RawDeadline{
			{Kind: "MANIFESTACAO", EndDate: t.AddDate(0, 0, 3), DaysRemaining: 3, Counting: "BUSINESS"},
		},
	}
}

// Degraded mode with an EMPTY process (no prazos, no andamentos) must still serialize
// key_dates_and_deadlines/recent_movements as [] — never null (the FE renders arrays).
func TestResume_NilGenerator_EmptyProcess_NoNullSlices(t *testing.T) {
	t.Parallel()

	ctxData := baseCtx()
	ctxData.OpenDeadlines = nil
	ctxData.RecentMovements = nil
	uc := NewResumoUseCase(fakeResumoReader{ctx: ctxData}, advisory.NewTemplateComposer(), nil, nil, nil, indexing.SearchDeps{}, "")

	view, err := uc.Resume(context.Background(), "t", "p")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if view.KeyDatesAndDeadlines == nil || len(view.KeyDatesAndDeadlines) != 0 {
		t.Errorf("key_dates_and_deadlines = %#v, want an empty (non-nil) slice", view.KeyDatesAndDeadlines)
	}
	if view.RecentMovements == nil || len(view.RecentMovements) != 0 {
		t.Errorf("recent_movements = %#v, want an empty (non-nil) slice", view.RecentMovements)
	}
	if view.Risks == nil || view.RecommendedActions == nil {
		t.Error("risks/recommended_actions must be non-nil in degraded mode")
	}

	// Prove the wire serialization is [] and never null.
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	for _, key := range []string{"key_dates_and_deadlines", "recent_movements", "risks", "recommended_actions"} {
		if strings.Contains(string(raw), `"`+key+`":null`) {
			t.Errorf("%s serialized as null: %s", key, raw)
		}
	}
}

// A nil generator (OpenRouter unconfigured) yields a deterministic DTO — no LLM call,
// no persist, no error. The FE still gets a structured payload it can render.
func TestResume_NilGenerator_Deterministic(t *testing.T) {
	t.Parallel()

	ctxData := baseCtx()
	uc := NewResumoUseCase(fakeResumoReader{ctx: ctxData}, advisory.NewTemplateComposer(), nil, nil, nil, indexing.SearchDeps{}, "")

	view, err := uc.Resume(context.Background(), "t", "p")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if view.Summary != "" {
		t.Errorf("summary = %q, want empty in degraded mode", view.Summary)
	}
	if view.CurrentStatus == "" {
		t.Error("current_status = empty, want the derived line in degraded mode")
	}
	if len(view.KeyDatesAndDeadlines) != 1 {
		t.Errorf("key_dates_and_deadlines = %d, want 1 (the open prazo)", len(view.KeyDatesAndDeadlines))
	}
	if len(view.RecentMovements) != 2 {
		t.Errorf("recent_movements = %d, want 2", len(view.RecentMovements))
	}
	if view.Risks == nil || len(view.Risks) != 0 {
		t.Errorf("risks = %v, want an empty (non-nil) slice", view.Risks)
	}
	if view.RecommendedActions == nil || len(view.RecommendedActions) != 0 {
		t.Errorf("recommended_actions = %v, want an empty (non-nil) slice", view.RecommendedActions)
	}
	if view.GeneratedAt.IsZero() {
		t.Error("generated_at = zero, want now")
	}
}

// Happy path: the reader supplies the context, the generator returns the
// schema-constrained JSON, and it parses into the structured view; the write-through
// store persists the FIRST summary best-effort.
func TestResume_HappyPath_ParsesAndPersists(t *testing.T) {
	t.Parallel()

	gen := &fakeResumoGen{out: []byte(`{"summary":"Processo de cobrança em fase de contestação.","current_status":"Aguardando manifestação da parte","key_dates_and_deadlines":[{"kind":"MANIFESTACAO","end_date":"2026-08-04","days_remaining":3,"urgency":"DUE_SOON","source":"deadline"}],"recent_movements":[{"occurred_at":"2026-08-01T12:00:00Z","text":"Partes intimadas para manifestação","source":"docket_entry"}],"risks":[{"description":"Prazo de manifestação próximo","source":"deadline"}],"recommended_actions":[{"action":"Elaborar a manifestação","source":"docket_entry"}]}`)}
	store := &fakeResumoStore{}
	uc := NewResumoUseCase(fakeResumoReader{ctx: baseCtx()}, advisory.NewTemplateComposer(), gen, store, nil, indexing.SearchDeps{}, "")

	view, err := uc.Resume(context.Background(), "t", "p")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if view.Summary == "" {
		t.Error("summary = empty, want the model's answer")
	}
	if len(view.Risks) != 1 || view.Risks[0].Description == "" {
		t.Errorf("risks = %v, want one parsed risk", view.Risks)
	}
	if len(view.RecommendedActions) != 1 {
		t.Errorf("recommended_actions = %d, want 1", len(view.RecommendedActions))
	}
	if view.GeneratedAt.IsZero() {
		t.Error("generated_at = zero, want now")
	}

	if store.calls != 1 {
		t.Fatalf("store calls = %d, want 1 (first open persists)", store.calls)
	}
	if store.gotTenantID != "t" || store.gotRecordID != "p" {
		t.Errorf("store ids = {%q, %q}, want {t, p}", store.gotTenantID, store.gotRecordID)
	}
	// The persisted JSON must be the model's own bytes (for later cache reads).
	if string(store.gotJSON) != string(gen.out) {
		t.Error("persisted JSON != model output")
	}
	if gen.gotReq.System == "" || gen.gotReq.User == "" {
		t.Error("generator got empty prompts")
	}
	if gen.gotReq.SchemaName != "process_resume" {
		t.Errorf("schema_name = %q, want process_resume", gen.gotReq.SchemaName)
	}
}

// Cache-hit: the context already carries a generated resume → serve it from the DB,
// never call the LLM, never persist again (write-once).
func TestResume_Cached_NoLLMCall(t *testing.T) {
	t.Parallel()

	genAt := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	cached := []byte(`{"summary":"Resumo em cache","current_status":"","key_dates_and_deadlines":[],"recent_movements":[],"risks":[],"recommended_actions":[]}`)
	ctxData := baseCtx()
	ctxData.AIResume = cached
	ctxData.AIResumeGeneratedAt = &genAt

	gen := &fakeResumoGen{out: []byte(`{"summary":"novo","current_status":"","key_dates_and_deadlines":[],"recent_movements":[],"risks":[],"recommended_actions":[]}`)}
	store := &fakeResumoStore{}
	uc := NewResumoUseCase(fakeResumoReader{ctx: ctxData}, advisory.NewTemplateComposer(), gen, store, nil, indexing.SearchDeps{}, "")

	view, err := uc.Resume(context.Background(), "t", "p")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if view.Summary != "Resumo em cache" {
		t.Errorf("summary = %q, want the cached value", view.Summary)
	}
	if !view.GeneratedAt.Equal(genAt) {
		t.Errorf("generated_at = %v, want the cached %v", view.GeneratedAt, genAt)
	}
	if gen.calls != 0 {
		t.Errorf("generator calls = %d, want 0 (cache hit)", gen.calls)
	}
	if store.calls != 0 {
		t.Errorf("store calls = %d, want 0 (write-once)", store.calls)
	}
}

// A store fault must never cost the lawyer the summary — log-and-keep the fresh answer.
func TestResume_StoreFault_KeepsFreshAnswer(t *testing.T) {
	t.Parallel()

	gen := &fakeResumoGen{out: []byte(`{"summary":"ok","current_status":"","key_dates_and_deadlines":[],"recent_movements":[],"risks":[],"recommended_actions":[]}`)}
	store := &fakeResumoStore{err: errors.New("db down")}
	uc := NewResumoUseCase(fakeResumoReader{ctx: baseCtx()}, advisory.NewTemplateComposer(), gen, store, nil, indexing.SearchDeps{}, "")

	view, err := uc.Resume(context.Background(), "t", "p")
	if err != nil {
		t.Fatalf("err = %v, want nil (store fault is best-effort)", err)
	}
	if view.Summary != "ok" {
		t.Errorf("summary = %q, want the fresh answer", view.Summary)
	}
}

// A reader miss/foreign error (the read's typed 404) propagates untouched.
func TestResume_ReaderError_Propagates(t *testing.T) {
	t.Parallel()

	uc := NewResumoUseCase(fakeResumoReader{err: ErrProcessoNotFound}, advisory.NewTemplateComposer(), nil, nil, nil, indexing.SearchDeps{}, "")
	_, err := uc.Resume(context.Background(), "t", "p")
	if !errors.Is(err, ErrProcessoNotFound) {
		t.Fatalf("err = %v, want ErrProcessoNotFound", err)
	}
}

// RAG best-effort: an embedder that FAILS must not break the summary — the use case
// logs and yields a context without chunks (never a 5xx).
func TestResume_EmbedderFault_SkipsRAG(t *testing.T) {
	t.Parallel()

	gen := &fakeResumoGen{out: []byte(`{"summary":"ok","current_status":"","key_dates_and_deadlines":[],"recent_movements":[],"risks":[],"recommended_actions":[]}`)}
	uc := NewResumoUseCase(
		fakeResumoReader{ctx: baseCtx()},
		advisory.NewTemplateComposer(),
		gen,
		nil,
		failingEmbedder{err: errors.New("voyage down")},
		indexing.SearchDeps{},
		"",
	)

	if _, err := uc.Resume(context.Background(), "t", "p"); err != nil {
		t.Fatalf("err = %v, want nil (RAG fault is best-effort)", err)
	}
	if gen.calls != 1 {
		t.Errorf("generator calls = %d, want 1 (summary still generated)", gen.calls)
	}
}

// failingEmbedder is an indexing.Embedder that always fails — the RAG best-effort
// fault path under test.
type failingEmbedder struct {
	err error
}

func (f failingEmbedder) Embed(_ context.Context, _ []string) ([][]float32, string, error) {
	return nil, "", f.err
}

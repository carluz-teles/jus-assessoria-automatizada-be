package acquisition

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jusassessoria/platform/internal/advisory"
	"github.com/jusassessoria/platform/lib/llm"
)

// fakeAnaliseReader is the analiseReader port fake — canned context or error.
type fakeAnaliseReader struct {
	ctx IntimacaoAnaliseCtx
	err error
}

func (f fakeAnaliseReader) GetIntimacaoAnaliseContext(_ context.Context, _, _ string) (IntimacaoAnaliseCtx, error) {
	return f.ctx, f.err
}

// fakeAnaliseGen is the llm.Generator fake for the analysis path.
type fakeAnaliseGen struct {
	out    []byte
	err    error
	calls  int
	gotReq llm.Request
}

func (f *fakeAnaliseGen) GenerateJSON(_ context.Context, req llm.Request) ([]byte, error) {
	f.calls++
	f.gotReq = req
	return f.out, f.err
}

func (f *fakeAnaliseGen) GenerateJSONStream(_ context.Context, req llm.Request, onChunk func(string) error) ([]byte, error) {
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

// fakeAnaliseStore records the persisted analysis and can be told to fail.
type fakeAnaliseStore struct {
	calls    int
	gotParam SaveAnaliseParams
	err      error
}

func (f *fakeAnaliseStore) SaveAnalise(_ context.Context, p SaveAnaliseParams) error {
	f.calls++
	f.gotParam = p
	return f.err
}

func analiseCtx() IntimacaoAnaliseCtx {
	return IntimacaoAnaliseCtx{
		Content:   "Fica a parte ré intimada para apresentar contestação no prazo de 15 dias.",
		Type:      strPtr("INTIMACAO"),
		CNJNumber: "0000001-23.2026.8.26.0001",
		Court:     "TJSP",
		Degree:    "G1",
		Class:     "PROCEDIMENTO_COMUM",
		Subject:   "Contrato",
	}
}

// providenciaJSON builds one schema-shaped providência item; opts overrides individual
// fields onto sane defaults so each test only spells out what it cares about.
func providenciaJSON(fields string) string {
	base := `{"title":"t","description":"d","suggested_assignee_user_id":null,"due_date":null,` +
		`"tipo":"contestar","gera_peca":true,"piece_profile_key":"contestacao","declarado":true,"confianca":null}`
	if fields == "" {
		return base
	}
	return strings.TrimSuffix(base, "}") + "," + fields + "}"
}

// Happy path: the generator returns schema-constrained JSON; it parses into the view and
// the store persists the summary + providência candidates.
func TestAnalisar_HappyPath_ParsesAndPersists(t *testing.T) {
	t.Parallel()

	gen := &fakeAnaliseGen{out: []byte(`{"summary":"A ré foi intimada para contestar em 15 dias.","providencias":[` +
		providenciaJSON("") + `]}`)}
	store := &fakeAnaliseStore{}
	uc := NewAnaliseUseCase(fakeAnaliseReader{ctx: analiseCtx()}, advisory.NewTemplateComposer(), gen, store, "")

	view, err := uc.Analisar(context.Background(), "t", "i")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if view.Summary == "" {
		t.Error("summary = empty, want the model's answer")
	}
	if len(view.Providencias) != 1 || view.Providencias[0].Title == "" {
		t.Errorf("providencias = %v, want one parsed item", view.Providencias)
	}
	if view.AnalyzedAt.IsZero() {
		t.Error("analyzed_at = zero, want now")
	}
	if gen.gotReq.SchemaName != "intimation_analysis" {
		t.Errorf("schema_name = %q, want intimation_analysis", gen.gotReq.SchemaName)
	}
	if gen.gotReq.System == "" || gen.gotReq.User == "" {
		t.Error("generator got empty prompts")
	}

	if store.calls != 1 {
		t.Fatalf("store calls = %d, want 1", store.calls)
	}
	if store.gotParam.TenantID != "t" || store.gotParam.IntimationID != "i" {
		t.Errorf("store ids = {%q, %q}, want {t, i}", store.gotParam.TenantID, store.gotParam.IntimationID)
	}
	if !store.gotParam.LogActivity {
		t.Error("logActivity = false, want true on the success path (INTIMATION_ANALYSIS_COMPLETED)")
	}
	if len(store.gotParam.Providencias) != 1 {
		t.Errorf("persisted providencias = %d, want 1", len(store.gotParam.Providencias))
	}
}

// Degraded (nil generator): no LLM call, persists+returns an empty analysis with a fresh
// analyzed_at (the FE moves to pós-análise and shows "IA indisponível"). The event still
// publishes (empty candidates) so the actionitem listener's guard aditivo runs.
func TestAnalisar_NilGenerator_PersistsDegraded(t *testing.T) {
	t.Parallel()

	store := &fakeAnaliseStore{}
	uc := NewAnaliseUseCase(fakeAnaliseReader{ctx: analiseCtx()}, advisory.NewTemplateComposer(), nil, store, "")

	view, err := uc.Analisar(context.Background(), "t", "i")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if view.Summary != "" {
		t.Errorf("summary = %q, want empty in degraded mode", view.Summary)
	}
	if view.Providencias == nil || len(view.Providencias) != 0 {
		t.Errorf("providencias = %#v, want an empty (non-nil) slice", view.Providencias)
	}
	if view.AnalyzedAt.IsZero() {
		t.Error("analyzed_at = zero, want now (degraded is still 'analysed')")
	}
	if store.calls != 1 || store.gotParam.Summary != "" || len(store.gotParam.Providencias) != 0 {
		t.Errorf("degraded persist = {calls:%d, summary:%q, prov:%v}, want {1, \"\", []}",
			store.calls, store.gotParam.Summary, store.gotParam.Providencias)
	}
	if store.gotParam.LogActivity {
		t.Error("logActivity = true, want false on the degraded path (no LLM configured)")
	}

	// The wire serialization must be [] and never null.
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if strings.Contains(string(raw), `"providencias":null`) {
		t.Errorf("providencias serialized as null: %s", raw)
	}
}

// An LLM fault degrades (log-not-fail): the analyze button never 5xx's — it persists+returns
// the empty analysis instead.
func TestAnalisar_LLMFault_Degrades(t *testing.T) {
	t.Parallel()

	gen := &fakeAnaliseGen{err: errors.New("openrouter 500")}
	store := &fakeAnaliseStore{}
	uc := NewAnaliseUseCase(fakeAnaliseReader{ctx: analiseCtx()}, advisory.NewTemplateComposer(), gen, store, "")

	view, err := uc.Analisar(context.Background(), "t", "i")
	if err != nil {
		t.Fatalf("err = %v, want nil (LLM fault degrades, never 5xx)", err)
	}
	if view.Summary != "" || len(view.Providencias) != 0 {
		t.Errorf("view = %#v, want the degraded empty analysis", view)
	}
	if store.calls != 1 {
		t.Errorf("store calls = %d, want 1 (degraded still persists)", store.calls)
	}
	if store.gotParam.LogActivity {
		t.Error("logActivity = true, want false on the degraded path (LLM fault)")
	}
}

// A malformed LLM payload also degrades (parse fault → empty analysis, never 5xx).
func TestAnalisar_MalformedJSON_Degrades(t *testing.T) {
	t.Parallel()

	gen := &fakeAnaliseGen{out: []byte(`not json`)}
	store := &fakeAnaliseStore{}
	uc := NewAnaliseUseCase(fakeAnaliseReader{ctx: analiseCtx()}, advisory.NewTemplateComposer(), gen, store, "")

	view, err := uc.Analisar(context.Background(), "t", "i")
	if err != nil {
		t.Fatalf("err = %v, want nil (parse fault degrades)", err)
	}
	if view.Summary != "" || len(view.Providencias) != 0 {
		t.Errorf("view = %#v, want the degraded empty analysis", view)
	}
	if store.gotParam.LogActivity {
		t.Error("logActivity = true, want false on the degraded path (parse fault)")
	}
}

// The providências list is capped so a runaway model output can't bloat the row/UI.
func TestAnalisar_CapsProvidencias(t *testing.T) {
	t.Parallel()

	items := make([]string, maxProvidencias+5)
	for i := range items {
		items[i] = providenciaJSON("")
	}
	payload := `{"summary":"s","providencias":[` + strings.Join(items, ",") + `]}`

	gen := &fakeAnaliseGen{out: []byte(payload)}
	uc := NewAnaliseUseCase(fakeAnaliseReader{ctx: analiseCtx()}, advisory.NewTemplateComposer(), gen, nil, "")

	view, err := uc.Analisar(context.Background(), "t", "i")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(view.Providencias) != maxProvidencias {
		t.Errorf("providencias = %d, want capped at %d", len(view.Providencias), maxProvidencias)
	}
}

// A store fault must never cost the lawyer the analysis — log-and-keep the fresh answer.
func TestAnalisar_StoreFault_KeepsFreshAnswer(t *testing.T) {
	t.Parallel()

	gen := &fakeAnaliseGen{out: []byte(`{"summary":"ok","providencias":[]}`)}
	store := &fakeAnaliseStore{err: errors.New("db down")}
	uc := NewAnaliseUseCase(fakeAnaliseReader{ctx: analiseCtx()}, advisory.NewTemplateComposer(), gen, store, "")

	view, err := uc.Analisar(context.Background(), "t", "i")
	if err != nil {
		t.Fatalf("err = %v, want nil (store fault is best-effort)", err)
	}
	if view.Summary != "ok" {
		t.Errorf("summary = %q, want the fresh answer", view.Summary)
	}
}

// Every generated providência resolves a valid suggested_assignee_user_id (matching a real
// member) and its name, and keeps a due_date ≤ the prazo end_date. This is the enriched
// shape the FE renders on the analysis card.
func TestAnalisar_EnrichesProvidencias(t *testing.T) {
	t.Parallel()

	ctx := analiseCtx()
	ctx.DeadlineEndDate = "2026-09-01"
	ctx.Members = []MemberCtx{{UserID: "u-luan", Name: "Luan"}, {UserID: "u-ana", Name: "Ana"}}

	gen := &fakeAnaliseGen{out: []byte(`{"summary":"s","providencias":[
		{"title":"Redigir defesa (art. 919, CPC)","description":"d","suggested_assignee_user_id":"u-ana","due_date":"2026-08-25",
		 "tipo":"contestar","gera_peca":true,"piece_profile_key":"contestacao","declarado":true,"confianca":null}
	]}`)}
	uc := NewAnaliseUseCase(fakeAnaliseReader{ctx: ctx}, advisory.NewTemplateComposer(), gen, nil, "")

	view, err := uc.Analisar(context.Background(), "t", "i")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	p := view.Providencias[0]
	if p.SuggestedAssigneeUserID == nil || *p.SuggestedAssigneeUserID != "u-ana" {
		t.Errorf("assignee id = %v, want u-ana", p.SuggestedAssigneeUserID)
	}
	if p.SuggestedAssigneeName == nil || *p.SuggestedAssigneeName != "Ana" {
		t.Errorf("assignee name = %v, want Ana", p.SuggestedAssigneeName)
	}
	if p.DueDate == nil || *p.DueDate != "2026-08-25" {
		t.Errorf("due_date = %v, want 2026-08-25", p.DueDate)
	}
}

// A hallucinated assignee id (not a real member) degrades to nil,nil, and a due_date AFTER the
// prazo end_date is dropped — the IA must never push a task past its legal horizon.
func TestAnalisar_RejectsBadAssigneeAndLateDueDate(t *testing.T) {
	t.Parallel()

	ctx := analiseCtx()
	ctx.DeadlineEndDate = "2026-09-01"
	ctx.Members = []MemberCtx{{UserID: "u-luan", Name: "Luan"}}

	gen := &fakeAnaliseGen{out: []byte(`{"summary":"s","providencias":[
		{"title":"t","description":"d","suggested_assignee_user_id":"u-ghost","due_date":"2026-09-15",
		 "tipo":"ciencia","gera_peca":false,"piece_profile_key":null,"declarado":false,"confianca":0.4}
	]}`)}
	uc := NewAnaliseUseCase(fakeAnaliseReader{ctx: ctx}, advisory.NewTemplateComposer(), gen, nil, "")

	view, err := uc.Analisar(context.Background(), "t", "i")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	p := view.Providencias[0]
	if p.SuggestedAssigneeUserID != nil {
		t.Errorf("assignee = %v, want nil (hallucinated id rejected)", *p.SuggestedAssigneeUserID)
	}
	if p.DueDate != nil {
		t.Errorf("due_date = %v, want nil (after end_date rejected)", *p.DueDate)
	}
}

// A teor declarado carries no confidence score, even if the model attaches one — the
// classification is a fact from the text, not an inference (docs §3).
func TestAnalisar_DeclaradoDropsConfianca(t *testing.T) {
	t.Parallel()

	gen := &fakeAnaliseGen{out: []byte(`{"summary":"s","providencias":[
		{"title":"t","description":"d","suggested_assignee_user_id":null,"due_date":null,
		 "tipo":"contestar","gera_peca":true,"piece_profile_key":"contestacao","declarado":true,"confianca":0.9}
	]}`)}
	uc := NewAnaliseUseCase(fakeAnaliseReader{ctx: analiseCtx()}, advisory.NewTemplateComposer(), gen, nil, "")

	view, err := uc.Analisar(context.Background(), "t", "i")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	p := view.Providencias[0]
	if !p.Declarado {
		t.Error("declarado = false, want true")
	}
	if p.Confianca != nil {
		t.Errorf("confianca = %v, want nil on a declarado item", *p.Confianca)
	}
	if p.Tipo != "contestar" || !p.GeraPeca || p.PieceProfileKey == nil || *p.PieceProfileKey != "contestacao" {
		t.Errorf("classification = %+v, want {contestar, true, contestacao}", p)
	}
}

// An IA-inferred (não declarado) providência keeps its confidence score, normalized
// (trimmed/lowercased) tipo, and its candidates are exactly what reaches the event —
// verified via the store's recorded ProvidenciaCandidate (candidatesFromView's contract).
func TestAnalisar_InferidoKeepsConfiancaAndNarrowsEventPayload(t *testing.T) {
	t.Parallel()

	gen := &fakeAnaliseGen{out: []byte(`{"summary":"s","providencias":[
		{"title":"t","description":"d","suggested_assignee_user_id":null,"due_date":null,
		 "tipo":"  Manifestar  ","gera_peca":false,"piece_profile_key":null,"declarado":false,"confianca":0.62}
	]}`)}
	store := &fakeAnaliseStore{}
	uc := NewAnaliseUseCase(fakeAnaliseReader{ctx: analiseCtx()}, advisory.NewTemplateComposer(), gen, store, "")

	view, err := uc.Analisar(context.Background(), "t", "i")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	p := view.Providencias[0]
	if p.Tipo != "manifestar" {
		t.Errorf("tipo = %q, want normalized %q", p.Tipo, "manifestar")
	}
	if p.Confianca == nil || *p.Confianca != 0.62 {
		t.Errorf("confianca = %v, want 0.62", p.Confianca)
	}

	if len(store.gotParam.Providencias) != 1 {
		t.Fatalf("event candidates = %d, want 1", len(store.gotParam.Providencias))
	}
	c := store.gotParam.Providencias[0]
	if c.Tipo != "manifestar" || c.GeraPeca || c.PieceProfileKey != nil || c.Declarado || c.Confianca == nil || *c.Confianca != 0.62 {
		t.Errorf("event candidate = %+v, want {manifestar, false, nil, false, 0.62}", c)
	}
}

// A reader miss/foreign error (the read's typed 404) propagates untouched (no LLM, no persist).
func TestAnalisar_ReaderError_Propagates(t *testing.T) {
	t.Parallel()

	gen := &fakeAnaliseGen{}
	store := &fakeAnaliseStore{}
	uc := NewAnaliseUseCase(fakeAnaliseReader{err: ErrIntimationNotFound}, advisory.NewTemplateComposer(), gen, store, "")

	_, err := uc.Analisar(context.Background(), "t", "i")
	if !errors.Is(err, ErrIntimationNotFound) {
		t.Fatalf("err = %v, want ErrIntimationNotFound", err)
	}
	if gen.calls != 0 || store.calls != 0 {
		t.Errorf("gen/store called on reader error: gen=%d store=%d", gen.calls, store.calls)
	}
}

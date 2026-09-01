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
	calls          int
	gotTenantID    string
	gotIntimID     string
	gotSummary     string
	gotAto         string
	gotProvJSON    []byte
	gotLogActivity bool
	err            error
}

func (f *fakeAnaliseStore) SaveAnalise(_ context.Context, tenantID, intimationID, summary, ato string, providencias []byte, logActivity bool) error {
	f.calls++
	f.gotTenantID = tenantID
	f.gotIntimID = intimationID
	f.gotSummary = summary
	f.gotAto = ato
	f.gotProvJSON = providencias
	f.gotLogActivity = logActivity
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

// Happy path: the generator returns schema-constrained JSON; it parses into the view and
// the store persists the summary + providências.
func TestAnalisar_HappyPath_ParsesAndPersists(t *testing.T) {
	t.Parallel()

	gen := &fakeAnaliseGen{out: []byte(`{"summary":"A ré foi intimada para contestar em 15 dias.","providencias":[{"title":"Protocolar contestação","description":"Elaborar e protocolar a contestação dentro do prazo legal."}]}`)}
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
	if store.gotTenantID != "t" || store.gotIntimID != "i" {
		t.Errorf("store ids = {%q, %q}, want {t, i}", store.gotTenantID, store.gotIntimID)
	}
	if !store.gotLogActivity {
		t.Error("logActivity = false, want true on the success path (INTIMATION_ANALYSIS_COMPLETED)")
	}
	// The persisted providências must be the parsed+sanitized list, as JSON.
	var persisted []IntimacaoProvidenciaView
	if err := json.Unmarshal(store.gotProvJSON, &persisted); err != nil {
		t.Fatalf("persisted providencias not valid JSON: %v", err)
	}
	if len(persisted) != 1 {
		t.Errorf("persisted providencias = %d, want 1", len(persisted))
	}
}

// Degraded (nil generator): no LLM call, persists+returns an empty analysis with a fresh
// analyzed_at (the FE moves to pós-análise and shows "IA indisponível").
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
	if store.calls != 1 || store.gotSummary != "" || string(store.gotProvJSON) != "[]" {
		t.Errorf("degraded persist = {calls:%d, summary:%q, prov:%s}, want {1, \"\", []}", store.calls, store.gotSummary, store.gotProvJSON)
	}
	if store.gotLogActivity {
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
	if store.gotLogActivity {
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
	if store.gotLogActivity {
		t.Error("logActivity = true, want false on the degraded path (parse fault)")
	}
}

// The providências list is capped so a runaway model output can't bloat the row/UI.
func TestAnalisar_CapsProvidencias(t *testing.T) {
	t.Parallel()

	items := make([]IntimacaoProvidenciaView, maxProvidencias+5)
	for i := range items {
		items[i] = IntimacaoProvidenciaView{Title: "t", Description: "d"}
	}
	payload, _ := json.Marshal(struct {
		Summary      string                     `json:"summary"`
		Providencias []IntimacaoProvidenciaView `json:"providencias"`
	}{Summary: "s", Providencias: items})

	gen := &fakeAnaliseGen{out: payload}
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

// Every generated providência is server-stamped SUGGESTED, a valid suggested_assignee_user_id
// (matching a real member) is kept + its name resolved, and a due_date ≤ the prazo end_date is
// kept. This is the enriched shape the FE renders on the analysis card.
func TestAnalisar_EnrichesProvidencias(t *testing.T) {
	t.Parallel()

	ctx := analiseCtx()
	ctx.DeadlineEndDate = "2026-09-01"
	ctx.Members = []MemberCtx{{UserID: "u-luan", Name: "Luan"}, {UserID: "u-ana", Name: "Ana"}}

	gen := &fakeAnaliseGen{out: []byte(`{"summary":"s","providencias":[
		{"title":"Redigir defesa (art. 919, CPC)","description":"d","suggested_assignee_user_id":"u-ana","due_date":"2026-08-25"}
	]}`)}
	uc := NewAnaliseUseCase(fakeAnaliseReader{ctx: ctx}, advisory.NewTemplateComposer(), gen, nil, "")

	view, err := uc.Analisar(context.Background(), "t", "i")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	p := view.Providencias[0]
	if p.Status != ProvidenciaStatusSuggested {
		t.Errorf("status = %q, want SUGGESTED", p.Status)
	}
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
		{"title":"t","description":"d","suggested_assignee_user_id":"u-ghost","due_date":"2026-09-15"}
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
	if p.Status != ProvidenciaStatusSuggested {
		t.Errorf("status = %q, want SUGGESTED", p.Status)
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

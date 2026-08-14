package deadline

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jusassessoria/platform/internal/advisory"
	"github.com/jusassessoria/platform/lib/llm"
)

type fakePrazoReader struct {
	view PrazoSuggestContext
	err  error
}

func (f fakePrazoReader) SuggestContext(context.Context, string, string) (PrazoSuggestContext, error) {
	return f.view, f.err
}

type fakeGen struct {
	out    []byte
	err    error
	gotReq llm.Request
}

func (f *fakeGen) GenerateJSON(_ context.Context, req llm.Request) ([]byte, error) {
	f.gotReq = req
	return f.out, f.err
}

type fakeStore struct {
	got   SuggestionRecord
	calls int
	err   error
}

func (f *fakeStore) SaveSuggestion(_ context.Context, _ string, rec SuggestionRecord) (string, error) {
	f.calls++
	f.got = rec
	return "sugg-id", f.err
}

// A nil generator (OpenRouter unconfigured) yields no suggestions and no error — the F2 form
// still opens.
func TestSuggestTasks_NilGenerator_Empty(t *testing.T) {
	uc := NewSuggestUseCase(fakePrazoReader{}, advisory.NewTemplateComposer(), nil, nil, "")
	sugg, err := uc.SuggestTasks(context.Background(), "t", "p")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if sugg.Summary != "" || sugg.Recommendation != "" {
		t.Errorf("summary/recommendation = {%q, %q}, want empty", sugg.Summary, sugg.Recommendation)
	}
	if sugg.Tasks == nil || len(sugg.Tasks) != 0 {
		t.Errorf("tasks = %v, want an empty (non-nil) slice", sugg.Tasks)
	}
}

// Happy path: the composer builds the prompt from the prazo's kind/days/counting, the generator
// returns the schema-constrained JSON, and it parses into the Suggestion (summary + recommendation
// + tasks, each with a description).
func TestSuggestTasks_HappyPath(t *testing.T) {
	gen := &fakeGen{out: []byte(`{"summary":"O réu foi citado para contestar.","recommendation":"Elaborar e protocolar a contestação em 15 dias úteis.","tasks":[{"title":"Redigir contestação","kind":"PECA","description":"Elaborar a peça de defesa."},{"title":"Protocolar","kind":"PROTOCOLO","description":"Protocolar a contestação no PJe."}]}`)}
	uc := NewSuggestUseCase(
		fakePrazoReader{view: PrazoSuggestContext{Kind: "CONTESTACAO", Days: 15, Counting: "BUSINESS"}},
		advisory.NewTemplateComposer(), gen, nil, "",
	)
	sugg, err := uc.SuggestTasks(context.Background(), "t", "p")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if sugg.Summary != "O réu foi citado para contestar." {
		t.Errorf("summary = %q", sugg.Summary)
	}
	if sugg.Recommendation != "Elaborar e protocolar a contestação em 15 dias úteis." {
		t.Errorf("recommendation = %q", sugg.Recommendation)
	}
	if len(sugg.Tasks) != 2 ||
		sugg.Tasks[0].Title != "Redigir contestação" ||
		sugg.Tasks[0].Kind != "PECA" ||
		sugg.Tasks[0].Description != "Elaborar a peça de defesa." {
		t.Errorf("tasks = %+v", sugg.Tasks)
	}
	// The composer's prompt + schema reached the generator with the case context injected.
	if gen.gotReq.SchemaName != "suggested_tasks" || len(gen.gotReq.Schema) == 0 {
		t.Errorf("request schema not set: name=%q schema_len=%d", gen.gotReq.SchemaName, len(gen.gotReq.Schema))
	}
	// The schema constrains the v2 shape: summary + recommendation (top-level) and a per-task
	// description are all required, so a compliant model can never omit them.
	for _, want := range []string{"summary", "recommendation", "description"} {
		if !strings.Contains(string(gen.gotReq.Schema), want) {
			t.Errorf("schema missing %q field\n---\n%s", want, gen.gotReq.Schema)
		}
	}
	if !strings.Contains(gen.gotReq.User, "CONTESTACAO") || !strings.Contains(gen.gotReq.User, "15 dias úteis") {
		t.Errorf("user prompt missing case context:\n%s", gen.gotReq.User)
	}
}

// The gap this slice closes: the richer case signals — the process's court/degree/class/subject
// and the intimação's type + teor — reach the LLM. With them populated, the composed user prompt
// carries each one (the composer renders "Tribunal: …", "Teor da intimação: …"), so the model
// grounds its summary/recommendation on the real intimação instead of just kind/days.
func TestSuggestTasks_InjectsRicherCaseContext(t *testing.T) {
	gen := &fakeGen{out: []byte(`{"summary":"s","recommendation":"r","tasks":[]}`)}
	uc := NewSuggestUseCase(
		fakePrazoReader{view: PrazoSuggestContext{
			Kind:           "CONTESTACAO",
			Days:           15,
			Counting:       "BUSINESS",
			Court:          "TJSP",
			Degree:         "G1",
			Class:          "Procedimento Comum Cível",
			Subject:        "Rescisão do Contrato",
			IntimationType: "CITACAO",
			IntimationText: "Fica o réu citado para apresentar defesa no prazo legal.",
		}},
		advisory.NewTemplateComposer(), gen, nil, "",
	)
	if _, err := uc.SuggestTasks(context.Background(), "t", "p"); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	for _, want := range []string{
		"TJSP",
		"G1",
		"Procedimento Comum Cível",
		"Rescisão do Contrato",
		"CITACAO",
		"Fica o réu citado para apresentar defesa no prazo legal.",
	} {
		if !strings.Contains(gen.gotReq.User, want) {
			t.Errorf("user prompt missing %q:\n%s", want, gen.gotReq.User)
		}
	}
}

// A prazo whose optional context is empty (NULL court_record columns, an intimação with no type,
// an empty teor) must NOT error — the composer just omits the empty labels. The suggestion still
// composes from kind/days/counting and the prompt carries no dangling "Tribunal:" / "Teor …:".
func TestSuggestTasks_EmptyContextFields_NoError(t *testing.T) {
	gen := &fakeGen{out: []byte(`{"summary":"","recommendation":"","tasks":[]}`)}
	uc := NewSuggestUseCase(
		fakePrazoReader{view: PrazoSuggestContext{Kind: "RECURSO", Days: 15, Counting: "BUSINESS"}},
		advisory.NewTemplateComposer(), gen, nil, "",
	)
	sugg, err := uc.SuggestTasks(context.Background(), "t", "p")
	if err != nil {
		t.Fatalf("err = %v, want nil (empty context is not an error)", err)
	}
	if sugg.Tasks == nil {
		t.Errorf("tasks = nil, want an empty (non-nil) slice")
	}
	if !strings.Contains(gen.gotReq.User, "RECURSO") {
		t.Errorf("user prompt missing the prazo kind:\n%s", gen.gotReq.User)
	}
	for _, absent := range []string{"Tribunal", "Grau", "Classe/rito", "Assunto", "Teor da intimação"} {
		if strings.Contains(gen.gotReq.User, absent) {
			t.Errorf("user prompt has a dangling %q label for an empty field:\n%s", absent, gen.gotReq.User)
		}
	}
}

// With a store configured, the suggester captures provenance (feedback loop, camada 1): the
// exact suggested tasks, the composed prompt_version and the model, keyed to the prazo +
// intimação — the raw material the confirm diffs against the human's choice.
func TestSuggestTasks_PersistsProvenance(t *testing.T) {
	gen := &fakeGen{out: []byte(`{"summary":"s","recommendation":"r","tasks":[{"title":"Redigir contestação","kind":"PECA","description":"d"}]}`)}
	store := &fakeStore{}
	uc := NewSuggestUseCase(
		fakePrazoReader{view: PrazoSuggestContext{Kind: "CONTESTACAO", Days: 15, Counting: "BUSINESS", IntimationID: "int-1"}},
		advisory.NewTemplateComposer(), gen, store, "openai/gpt-4o-mini",
	)
	if _, err := uc.SuggestTasks(context.Background(), "t", "prazo-1"); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if store.calls != 1 {
		t.Fatalf("SaveSuggestion calls = %d, want 1", store.calls)
	}
	if store.got.DeadlineID != "prazo-1" || store.got.IntimationID != "int-1" {
		t.Errorf("provenance ids = {%q, %q}, want {prazo-1, int-1}", store.got.DeadlineID, store.got.IntimationID)
	}
	if store.got.Model != "openai/gpt-4o-mini" || store.got.PromptVersion == "" {
		t.Errorf("provenance model/version = {%q, %q}", store.got.Model, store.got.PromptVersion)
	}
	if len(store.got.Tasks) != 1 || store.got.Tasks[0].Title != "Redigir contestação" {
		t.Errorf("persisted tasks = %+v", store.got.Tasks)
	}
}

// A store fault must NOT break the F2: the suggestions still return (best-effort provenance).
func TestSuggestTasks_StoreErrorIsNonFatal(t *testing.T) {
	gen := &fakeGen{out: []byte(`{"summary":"s","recommendation":"r","tasks":[{"title":"Protocolar","kind":"PROTOCOLO","description":"d"}]}`)}
	store := &fakeStore{err: errors.New("db down")}
	uc := NewSuggestUseCase(
		fakePrazoReader{view: PrazoSuggestContext{Kind: "CONTESTACAO", Days: 15, Counting: "BUSINESS"}},
		advisory.NewTemplateComposer(), gen, store, "m",
	)
	sugg, err := uc.SuggestTasks(context.Background(), "t", "p")
	if err != nil {
		t.Fatalf("err = %v, want nil (store fault is non-fatal)", err)
	}
	if len(sugg.Tasks) != 1 {
		t.Errorf("tasks = %v, want the suggestion despite the store error", sugg.Tasks)
	}
}

// A missing prazo surfaces the read's typed not-found (→ 404 at the edge).
func TestSuggestTasks_PrazoNotFound(t *testing.T) {
	uc := NewSuggestUseCase(fakePrazoReader{err: ErrDeadlineNotFound}, advisory.NewTemplateComposer(), &fakeGen{}, nil, "")
	if _, err := uc.SuggestTasks(context.Background(), "t", "p"); err == nil {
		t.Fatal("err = nil, want ErrDeadlineNotFound")
	}
}

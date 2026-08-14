package deadline

import (
	"context"
	"strings"
	"testing"

	"github.com/jusassessoria/platform/internal/advisory"
	"github.com/jusassessoria/platform/lib/llm"
)

type fakePrazoReader struct {
	view PrazoDetailView
	err  error
}

func (f fakePrazoReader) Prazo(context.Context, string, string) (PrazoDetailView, error) {
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

// A nil generator (OpenRouter unconfigured) yields no suggestions and no error — the F2 form
// still opens.
func TestSuggestTasks_NilGenerator_Empty(t *testing.T) {
	uc := NewSuggestUseCase(fakePrazoReader{}, advisory.NewTemplateComposer(), nil)
	tasks, err := uc.SuggestTasks(context.Background(), "t", "p")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(tasks) != 0 {
		t.Errorf("tasks = %v, want empty", tasks)
	}
}

// Happy path: the composer builds the prompt from the prazo's kind/days/counting, the generator
// returns the schema-constrained JSON, and it parses into SuggestedTask.
func TestSuggestTasks_HappyPath(t *testing.T) {
	gen := &fakeGen{out: []byte(`{"tasks":[{"title":"Redigir contestação","kind":"PECA"},{"title":"Protocolar","kind":"PROTOCOLO"}]}`)}
	uc := NewSuggestUseCase(
		fakePrazoReader{view: PrazoDetailView{Kind: "CONTESTACAO", Days: 15, Counting: "BUSINESS"}},
		advisory.NewTemplateComposer(), gen,
	)
	tasks, err := uc.SuggestTasks(context.Background(), "t", "p")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(tasks) != 2 || tasks[0].Title != "Redigir contestação" || tasks[0].Kind != "PECA" {
		t.Errorf("tasks = %+v", tasks)
	}
	// The composer's prompt + schema reached the generator with the case context injected.
	if gen.gotReq.SchemaName != "suggested_tasks" || len(gen.gotReq.Schema) == 0 {
		t.Errorf("request schema not set: name=%q schema_len=%d", gen.gotReq.SchemaName, len(gen.gotReq.Schema))
	}
	if !strings.Contains(gen.gotReq.User, "CONTESTACAO") || !strings.Contains(gen.gotReq.User, "15 dias úteis") {
		t.Errorf("user prompt missing case context:\n%s", gen.gotReq.User)
	}
}

// A missing prazo surfaces the read's typed not-found (→ 404 at the edge).
func TestSuggestTasks_PrazoNotFound(t *testing.T) {
	uc := NewSuggestUseCase(fakePrazoReader{err: ErrDeadlineNotFound}, advisory.NewTemplateComposer(), &fakeGen{})
	if _, err := uc.SuggestTasks(context.Background(), "t", "p"); err == nil {
		t.Fatal("err = nil, want ErrDeadlineNotFound")
	}
}

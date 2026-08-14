package deadline

import (
	"context"
	"encoding/json"

	"github.com/jusassessoria/platform/internal/advisory"
	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/llm"
)

// suggest.go is the on-demand "intimação → tarefas sugeridas" read use case (fatia 5 do
// erd-ai-advisory). It reads the derived prazo, composes the instruction-set via the versioned
// meta-prompt framework (internal/advisory), asks the LLM (lib/llm, structured output) for the
// tasks, and returns them to pre-fill the F2 "Aprovar tudo" form. It is READ-side (no tx): it
// only reads a prazo + calls the model, so it lives off the pool like the other reads. The
// generator is optional — when OpenRouter is unconfigured (no key), it returns no suggestions so
// the F2 form still works (the lawyer types the tasks manually).

// SuggestedTask is one AI-suggested action for the F2 form: a short imperative title + a short
// kind (ANALISE|PECA|PROTOCOLO|PROVIDENCIA|CIENCIA…). It mirrors the ConfirmTaskInput shape the
// form submits, so the FE drops each suggestion straight into a task row.
type SuggestedTask struct {
	Title string `json:"title"`
	Kind  string `json:"kind"`
}

// prazoReader is the narrow read the suggester needs — one prazo detail by id, tenant-scoped.
// *ReadUseCase satisfies it, so the worker composes the existing read use case here.
type prazoReader interface {
	Prazo(ctx context.Context, tenantID, id string) (PrazoDetailView, error)
}

// SuggestUseCase composes the meta-prompt for the case and calls the LLM. It depends only on
// ports (the prazo reader, the prompt composer, the generator) so tests inject fakes and the LLM
// is never hit under test.
type SuggestUseCase struct {
	reader   prazoReader
	composer advisory.PromptComposer
	gen      llm.Generator // optional: nil when OpenRouter is unconfigured
}

// NewSuggestUseCase wires the suggester. gen may be nil (no LLM configured) — SuggestTasks then
// returns no suggestions instead of failing, so the F2 form degrades gracefully.
func NewSuggestUseCase(reader prazoReader, composer advisory.PromptComposer, gen llm.Generator) *SuggestUseCase {
	return &SuggestUseCase{reader: reader, composer: composer, gen: gen}
}

// suggestTasksSchema constrains the model's output to { tasks: [ { title, kind } ] } via
// OpenRouter's json_schema structured output (strict). additionalProperties:false + required make
// the shape exact, so the parse below never sees a surprise field.
var suggestTasksSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "tasks": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "title": { "type": "string" },
          "kind":  { "type": "string" }
        },
        "required": ["title", "kind"],
        "additionalProperties": false
      }
    }
  },
  "required": ["tasks"],
  "additionalProperties": false
}`)

// SuggestTasks returns the AI-suggested tasks for a prazo. A missing/foreign prazo → the read's
// typed ErrDeadlineNotFound (→ 404). With no generator configured it returns an empty slice (no
// error) so the F2 form still opens. An LLM/parse fault surfaces typed (the handler maps it).
func (uc *SuggestUseCase) SuggestTasks(ctx context.Context, tenantID, prazoID string) ([]SuggestedTask, error) {
	if uc.gen == nil {
		return []SuggestedTask{}, nil
	}

	prazo, err := uc.reader.Prazo(ctx, tenantID, prazoID)
	if err != nil {
		return nil, err
	}

	composed, err := uc.composer.Compose(advisory.AgentSuggestTasks, advisory.CaseContext{
		PrazoKind: prazo.Kind,
		PrazoDays: prazo.Days,
		Counting:  prazo.Counting,
		// Court/Class/IntimationType/IntimationText: richer signals are a later enhancement;
		// the composer omits the empty ones, so v0 suggests from kind/days/counting.
	})
	if err != nil {
		return nil, err
	}

	out, err := uc.gen.GenerateJSON(ctx, llm.Request{
		System:     composed.System,
		User:       composed.User,
		Schema:     suggestTasksSchema,
		SchemaName: "suggested_tasks",
	})
	if err != nil {
		return nil, err
	}

	var parsed struct {
		Tasks []SuggestedTask `json:"tasks"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return nil, apperr.NewInfra("deadline: parse suggested tasks", err)
	}
	if parsed.Tasks == nil {
		parsed.Tasks = []SuggestedTask{}
	}
	return parsed.Tasks, nil
}

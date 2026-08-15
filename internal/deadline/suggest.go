package deadline

import (
	"context"
	"encoding/json"
	"log/slog"

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

// SuggestedTask is one AI-suggested action for the F2 form: a short imperative title, a short
// kind and a short actionable description. It mirrors the task shape the form submits (POST
// /v1/tasks), so the FE drops each suggestion straight into a task row. The kind carries the
// closed TaskKind taxonomy (entity.go: ANALISE|PECA|PROTOCOLO|PROVIDENCIA|CIENCIA) — the same
// set the advisory prompt constrains the model to and the task edge validation enforces.
type SuggestedTask struct {
	Title       string `json:"title"`
	Kind        string `json:"kind"`
	Description string `json:"description"`
}

// Suggestion is the whole answer of one suggest_tasks LLM call: the "O que aconteceu" summary
// (what the intimação communicates), the "O que fazer" recommendation (the recommended next
// steps) and the list of suggested tasks. Summary/Recommendation are response-only (pass-through):
// they are NOT persisted as provenance — only Tasks is (see SaveSuggestion). Both strings may be
// empty ("") when the LLM is unconfigured, which is a valid, non-error answer.
type Suggestion struct {
	Summary        string
	Recommendation string
	Tasks          []SuggestedTask
}

// prazoReader is the narrow read the suggester needs — one prazo's advisory case context by id,
// tenant-scoped (prazo signals + the process's court_record + the origin intimação's teor, plus
// any already-persisted ai_summary/ai_recommendation). *ReadUseCase satisfies it, so the worker
// composes the existing read use case here.
type prazoReader interface {
	SuggestContext(ctx context.Context, tenantID, id string) (PrazoSuggestContext, error)
}

// aiSummaryStore persists the "O que aconteceu"/"O que fazer" summary (migration 0036) the FIRST
// time a prazo is summarized — sync-on-first-GET, write-once (no invalidation path). Like
// SuggestionStore it is a separate port from the write Repository because it writes on the READ
// path and the persist is best-effort: a failure here must never break the F2 pre-fill. Its
// implementation (pgAISummaryStore) owns its own unit of work, mirroring pgSuggestionStore.
type aiSummaryStore interface {
	SaveAISummary(ctx context.Context, tenantID, deadlineID, summary, recommendation string) error
}

// SuggestUseCase composes the meta-prompt for the case and calls the LLM. It depends only on
// ports (the prazo reader, the prompt composer, the generator) so tests inject fakes and the LLM
// is never hit under test.
type SuggestUseCase struct {
	reader       prazoReader
	composer     advisory.PromptComposer
	gen          llm.Generator   // optional: nil when OpenRouter is unconfigured
	store        SuggestionStore // optional: nil skips provenance capture (feedback loop, camada 1)
	summaryStore aiSummaryStore  // optional: nil skips the ai_summary write-through (migration 0036)
	model        string          // provenance: the model the generator answers with by default
}

// NewSuggestUseCase wires the suggester. gen may be nil (no LLM configured) — SuggestTasks then
// returns no suggestions instead of failing, so the F2 form degrades gracefully. store may be
// nil (no provenance capture); when present, each suggestion batch is persisted (best-effort)
// so the confirm can later diff it against the human's choice (feedback loop). summaryStore may
// be nil (no summary persistence); when present, the FIRST summary/recommendation for a prazo is
// written through (best-effort) so later opens serve it from the DB instead of asking the LLM
// again. model is the generator's default model, recorded as provenance alongside the composed
// prompt_version.
func NewSuggestUseCase(reader prazoReader, composer advisory.PromptComposer, gen llm.Generator, store SuggestionStore, summaryStore aiSummaryStore, model string) *SuggestUseCase {
	return &SuggestUseCase{reader: reader, composer: composer, gen: gen, store: store, summaryStore: summaryStore, model: model}
}

// suggestTasksSchema constrains the model's output to
// { summary, recommendation, tasks: [ { title, kind, description } ] } via OpenRouter's
// json_schema structured output (strict). additionalProperties:false + required at both levels
// make the shape exact, so the parse below never sees a surprise field and the three v2 outputs
// (summary/recommendation/description) can never be silently omitted.
var suggestTasksSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "summary":        { "type": "string" },
    "recommendation": { "type": "string" },
    "tasks": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "title":       { "type": "string" },
          "kind":        { "type": "string" },
          "description": { "type": "string" }
        },
        "required": ["title", "kind", "description"],
        "additionalProperties": false
      }
    }
  },
  "required": ["summary", "recommendation", "tasks"],
  "additionalProperties": false
}`)

// SuggestTasks returns the AI Suggestion for a prazo — the summary, the recommendation and the
// suggested tasks, from one LLM call. A missing/foreign prazo → the read's typed
// ErrDeadlineNotFound (→ 404). With no generator configured it returns an empty Suggestion (empty
// strings + empty task slice, no error) so the F2 form still opens. An LLM/parse fault surfaces
// typed (the handler maps it).
//
// Summary/Recommendation persistence (migration 0036, sync-on-first-GET): the suggested TASKS are
// ALWAYS regenerated fresh on every call (the LLM is always asked), but the summary/recommendation
// freeze after the first successful answer — a prazo whose PrazoSuggestContext already carries
// AISummaryGeneratedAt serves the PERSISTED summary/recommendation instead of the model's fresh
// (and possibly slightly different) retelling, so "O que aconteceu" reads identically across
// opens. The first time, the fresh answer is written through (best-effort). No invalidation path
// exists — this is write-once by design.
func (uc *SuggestUseCase) SuggestTasks(ctx context.Context, tenantID, prazoID string) (Suggestion, error) {
	if uc.gen == nil {
		return Suggestion{Tasks: []SuggestedTask{}}, nil
	}

	prazo, err := uc.reader.SuggestContext(ctx, tenantID, prazoID)
	if err != nil {
		return Suggestion{}, err
	}

	// The richest signals — the process's court/degree/class/subject and the intimação's own teor —
	// specialize the instruction-set (erd-ai-advisory.md §3). Any that come empty (a NULL column,
	// an intimação without a type) the composer simply omits, so a sparse case still composes.
	composed, err := uc.composer.Compose(advisory.AgentSuggestTasks, advisory.CaseContext{
		Court:          prazo.Court,
		Degree:         prazo.Degree,
		Class:          prazo.Class,
		Subject:        prazo.Subject,
		IntimationType: prazo.IntimationType,
		IntimationText: prazo.IntimationText,
		PrazoKind:      prazo.Kind,
		PrazoDays:      prazo.Days,
		Counting:       prazo.Counting,
	})
	if err != nil {
		return Suggestion{}, err
	}

	out, err := uc.gen.GenerateJSON(ctx, llm.Request{
		System:     composed.System,
		User:       composed.User,
		Schema:     suggestTasksSchema,
		SchemaName: "suggested_tasks",
	})
	if err != nil {
		return Suggestion{}, err
	}

	var parsed struct {
		Summary        string          `json:"summary"`
		Recommendation string          `json:"recommendation"`
		Tasks          []SuggestedTask `json:"tasks"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return Suggestion{}, apperr.NewInfra("deadline: parse suggested tasks", err)
	}
	if parsed.Tasks == nil {
		parsed.Tasks = []SuggestedTask{}
	}

	// Summary/recommendation: serve the PERSISTED text once it exists (frozen at the first
	// successful answer — AISummaryGeneratedAt is the single source of truth for "already
	// persisted"); otherwise take the fresh answer and write it through, best-effort, so the
	// NEXT open serves the cache instead of asking the LLM again. A store fault must never cost
	// the lawyer the suggestions — log and keep the fresh (unpersisted) answer.
	summary, recommendation := parsed.Summary, parsed.Recommendation
	switch {
	case prazo.AISummaryGeneratedAt != nil:
		summary, recommendation = prazo.AISummary, prazo.AIRecommendation
	case uc.summaryStore != nil:
		if err := uc.summaryStore.SaveAISummary(ctx, tenantID, prazoID, parsed.Summary, parsed.Recommendation); err != nil {
			slog.WarnContext(ctx, "deadline: persist ai summary failed",
				slog.String("prazo_id", prazoID), slog.Any("error", err))
		}
	}

	// Feedback loop (camada 1): capture WHAT the IA suggested, with WHICH prompt_version and
	// model, so the confirm can later measure the delta against the human's choice. Best-effort
	// by design: this is a GET that pre-fills a form, and provenance is a background concern —
	// a persist fault must never cost the lawyer the suggestions, so we log and return them. Only
	// the tasks are provenance; summary/recommendation are response-only (pass-through).
	if uc.store != nil && len(parsed.Tasks) > 0 {
		if _, err := uc.store.SaveSuggestion(ctx, tenantID, SuggestionRecord{
			DeadlineID:    prazoID,
			IntimationID:  prazo.IntimationID,
			PromptVersion: composed.PromptVersion,
			Model:         uc.model,
			Tasks:         parsed.Tasks,
		}); err != nil {
			slog.WarnContext(ctx, "deadline: persist suggestion provenance failed",
				slog.String("prazo_id", prazoID), slog.Any("error", err))
		}
	}

	return Suggestion{
		Summary:        summary,
		Recommendation: recommendation,
		Tasks:          parsed.Tasks,
	}, nil
}

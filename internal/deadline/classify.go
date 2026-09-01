package deadline

import (
	"context"
	"encoding/json"

	"github.com/jusassessoria/platform/internal/advisory"
	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/llm"
)

// classify.go is the ingest-time "omissa intimação → tipo de ato" fallback (docs/design-motor-
// de-prazos-v1.md §"Fallback IA") — the ONLY point of IA in the whole motor de prazos. It NEVER
// computes a date, only a tipo + confiança, which OnIntimationObserved (domain.go) uses solely to
// flip Origem to "ia" (forcing the selo=a_apurar floor) and to record provenance in calc_memory
// (ia_tipo_inferido/ia_confianca). The deterministic date keeps coming from the V0 rule table
// (ResolveRule), untouched — mirrors suggest.go's SuggestUseCase shape (composer + optional gen).

// ClassifiedType is the tipo classification's answer — never a date (design §"nunca chuta").
type ClassifiedType struct {
	Tipo        string
	Confianca   float64
	Alternativa string
}

// ClassifyUseCase composes the classify_intimation_type instruction-set and asks the LLM. It
// depends only on ports (composer + generator), mirroring SuggestUseCase, so tests inject fakes
// and the real API is never hit under test.
type ClassifyUseCase struct {
	composer advisory.PromptComposer
	gen      llm.Generator // optional: nil when OpenRouter is unconfigured
	model    string        // provenance: the model the generator answers with by default
}

// NewClassifyUseCase wires the classifier. gen may be nil — ClassifyType then returns
// ErrClassifierUnavailable so OnIntimationObserved degrades gracefully (never chuta,
// docs/design-motor-de-prazos-v1.md §"Fallback IA": "se indisponível, o prazo omisso permanece
// a_apurar sem tipo inferido").
func NewClassifyUseCase(composer advisory.PromptComposer, gen llm.Generator, model string) *ClassifyUseCase {
	return &ClassifyUseCase{composer: composer, gen: gen, model: model}
}

// classifyTypeSchema constrains the model's output to { tipo, confianca, alternativa } via
// OpenRouter's json_schema structured output (strict), mirroring suggestTasksSchema in
// suggest.go. additionalProperties:false + required make the shape exact.
var classifyTypeSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "tipo":        { "type": "string" },
    "confianca":   { "type": "number" },
    "alternativa": { "type": "string" }
  },
  "required": ["tipo", "confianca", "alternativa"],
  "additionalProperties": false
}`)

// ClassifyType classifies an omissa intimação's tipo de ato. A nil generator (unconfigured LLM)
// or an empty `tipo` answer (the model itself declined to guess, per the prompt's instruction)
// both return ErrClassifierUnavailable — never a guessed answer.
func (uc *ClassifyUseCase) ClassifyType(ctx context.Context, tenantID string, c advisory.CaseContext) (ClassifiedType, error) {
	if uc.gen == nil {
		return ClassifiedType{}, ErrClassifierUnavailable
	}

	composed, err := uc.composer.Compose(advisory.AgentClassifyIntimationType, c)
	if err != nil {
		return ClassifiedType{}, err
	}

	out, err := uc.gen.GenerateJSON(ctx, llm.Request{
		System:     composed.System,
		User:       composed.User,
		Schema:     classifyTypeSchema,
		SchemaName: "classify_intimation_type",
		UseCase:    "deadline.classify_type",
		TenantID:   tenantID,
	})
	if err != nil {
		return ClassifiedType{}, err
	}

	var parsed ClassifiedType
	if err := json.Unmarshal(out, &parsed); err != nil {
		return ClassifiedType{}, apperr.NewInfra("deadline: parse classified type", err)
	}
	if parsed.Tipo == "" {
		return ClassifiedType{}, ErrClassifierUnavailable
	}
	return parsed, nil
}

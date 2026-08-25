package draft

// theses.go implements the synchronous Sugerir Teses use case (POST /v1/pecas/:id/theses).
// The advogado triggers this to get candidate legal theses grounded in the case corpus,
// BEFORE (or instead of) triggering Gerar. It is STATELESS: read + LLM only — no writer,
// no saga_state transition, no persisted row. Modeled on review.go's 2-phase shape
// (review.go has 3 phases because it persists a Review row; this use case stops after
// the LLM call).
//
//	Phase 1 (READ, short tx): GetDraftByID (tenant guard) + GetIntimationForDraft
//	                           (optional — non-fatal if missing) + GetPartiesForDraft.
//	Phase 2 (LLM, NO tx):     runRAG → buildDraftContext → ComposeTheses → GenerateJSON.
//
// On LLM failure the use case returns a typed error and touches NOTHING — there is no
// writer/write port on ThesesUseCase, so there is no state to leave inconsistent
// (an architectural impossibility, not just a choice not to write).

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/jusassessoria/platform/internal/advisory"
	"github.com/jusassessoria/platform/internal/indexing"
	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/llm"
)

// SuggestThesesCommand is the input for POST /v1/pecas/:id/theses.
type SuggestThesesCommand struct {
	TenantID string
	DraftID  string
}

// SuggestThesesResult is the output of SuggestTheses.
type SuggestThesesResult struct {
	Theses []Thesis
}

// thesesSchema is the JSON Schema constraining the LLM's output for Sugerir Teses
// (strict mode). Reference is a plain string (not a structured Citation) — a
// jurisprudência or dispositivo legal is not a chunk anchored in the case corpus.
var thesesSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "theses": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "label":      { "type": "string" },
          "confidence": { "type": "string", "enum": ["alta", "media", "baixa"] },
          "reference":  { "type": "string" },
          "foundation": { "type": "string" }
        },
        "required": ["label", "confidence", "reference", "foundation"],
        "additionalProperties": false
      }
    }
  },
  "required": ["theses"],
  "additionalProperties": false
}`)

// thesesOutput is the LLM's structured response for Sugerir Teses.
type thesesOutput struct {
	Theses []Thesis `json:"theses"`
}

// ThesesUseCase is the synchronous, stateless thesis-suggestion use case. It has NO
// writer — the ports below are exactly generationDepsReader's shape (reused, not
// duplicated) plus the LLM/RAG deps ReviewUseCase also carries.
type ThesesUseCase struct {
	uow      database.UnitOfWork
	reader   generationDepsReader
	gen      llm.Generator // nil → apperr Invalid "IA não configurada"
	emb      embedder      // nil → degraded (no RAG grounding)
	search   indexing.SearchDeps
	composer advisory.PromptComposer
	model    string
}

// ThesesUseCaseParams groups the construction parameters. All pointer fields may be nil.
type ThesesUseCaseParams struct {
	UoW      database.UnitOfWork
	Reader   generationDepsReader
	Gen      llm.Generator
	Emb      embedder
	Search   indexing.SearchDeps
	Composer advisory.PromptComposer
	Model    string // OpenRouter model slug; empty → generationModel fallback
}

// NewThesesUseCase wires the thesis-suggestion use case.
func NewThesesUseCase(p ThesesUseCaseParams) *ThesesUseCase {
	model := p.Model
	if model == "" {
		model = generationModel
	}
	return &ThesesUseCase{
		uow:      p.UoW,
		reader:   p.Reader,
		gen:      p.Gen,
		emb:      p.Emb,
		search:   p.Search,
		composer: p.Composer,
		model:    model,
	}
}

// SuggestTheses implements POST /v1/pecas/:id/theses.
//
// Phase 1 (READ, short tx): load draft + intimation (optional) + parties (optional).
// Phase 2 (LLM, NO tx):    RAG → compose → generate → decode.
//
// On any failure (nil generator, LLM error, parse error) the returned error is typed;
// there is no writer to roll back — the draft's saga_state is never touched.
func (uc *ThesesUseCase) SuggestTheses(ctx context.Context, cmd SuggestThesesCommand) (*SuggestThesesResult, error) {
	if uc.gen == nil {
		return nil, apperr.NewInvalid("IA não configurada: OPENROUTER_API_KEY não definida")
	}

	// ── Phase 1: READ (short tx) ──────────────────────────────────────────────
	var d *Draft
	var intimation *IntimationContext
	var parties []PartyInfo
	if err := uc.uow.Do(ctx, cmd.TenantID, func(tx database.Tx) error {
		loaded, err := uc.reader.GetDraftByID(ctx, tx, cmd.TenantID, cmd.DraftID)
		if err != nil {
			return err
		}
		d = loaded

		if d.IntimationID != "" {
			i, e := uc.reader.GetIntimationForDraft(ctx, tx, cmd.TenantID, d.IntimationID)
			if e != nil {
				slog.WarnContext(ctx, "theses: intimation load failed — falling back to tenant-wide RAG",
					slog.String("draft_id", cmd.DraftID),
					slog.Any("error", e),
				)
			} else {
				intimation = i
			}
		}
		if d.CaseID != "" {
			pp, e := uc.reader.GetPartiesForDraft(ctx, tx, cmd.TenantID, d.CaseID)
			if e != nil {
				slog.WarnContext(ctx, "theses: parties load failed",
					slog.String("draft_id", cmd.DraftID), slog.Any("error", e))
			} else {
				parties = pp
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}

	// ── Phase 2: LLM (NO tx — connection released) ───────────────────────────
	var crid *string
	if intimation != nil && intimation.CourtRecordID != "" {
		crid = &intimation.CourtRecordID
	}
	queryText := buildQueryText(d, intimation)
	chunks, _, grounded := runRAG(ctx, uc.emb, uc.search, cmd.TenantID, crid, queryText, 8)

	draftCtx := buildDraftContext(d, intimation, parties, chunks)
	composed, err := uc.composer.ComposeTheses(advisory.AgentSuggestTheses, draftCtx)
	if err != nil {
		return nil, fmt.Errorf("theses: compose prompt: %w", err)
	}

	rawBytes, err := uc.gen.GenerateJSON(ctx, llm.Request{
		System:     composed.System,
		User:       composed.User,
		Schema:     thesesSchema,
		SchemaName: "suggest_theses",
		Model:      uc.model,
		MaxTokens:  2048,
	})
	if err != nil {
		return nil, fmt.Errorf("theses: llm call: %w", err)
	}

	var out thesesOutput
	if err := json.Unmarshal(rawBytes, &out); err != nil {
		return nil, fmt.Errorf("theses: parse llm output: %w", err)
	}

	theses := out.Theses
	if theses == nil {
		theses = []Thesis{}
	}

	slog.InfoContext(ctx, "draft theses suggestion completed",
		slog.String("draft_id", cmd.DraftID),
		slog.Int("theses", len(theses)),
		slog.Bool("grounded", grounded),
	)
	return &SuggestThesesResult{Theses: theses}, nil
}

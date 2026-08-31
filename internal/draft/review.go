package draft

// review.go implements the synchronous Revisar use case (POST /v1/pecas/:id/review).
// The advogado triggers this after Gerar produces the minuta; it produces AI suggestions
// (findings) grounded in the case corpus, without touching the minuta content.
//
// Structured in 3 phases to avoid holding a pgx connection open during the LLM call:
//
//	Phase 1 (READ, short tx):   GetDraftByID (tenant guard + content guard + saga guard)
//	                             + GetIntimationForDraft (optional — non-fatal if missing).
//	Phase 2 (LLM, NO tx):       runRAG → ComposeReview → GenerateJSON → buildFindings.
//	Phase 3 (WRITE, short tx):  InsertReview + UpdateSagaState → REVIEWED.
//
// On LLM failure the draft stays in its current state (DRAFTED or REVIEWED); no FAILED
// is written — the advogado can retry without re-generating the minuta.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jusassessoria/platform/internal/advisory"
	"github.com/jusassessoria/platform/internal/indexing"
	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/llm"
)

// ReviewDraftCommand is the input for POST /v1/pecas/:id/review.
type ReviewDraftCommand struct {
	TenantID string
	DraftID  string
}

// ReviewResult is the output of ReviewDraft: the persisted review and the updated saga state.
type ReviewResult struct {
	Review    *Review
	SagaState string
}

// reviewDepsReader is the narrow read port the ReviewUseCase needs. Reuses the same methods
// as generationDepsReader — same satisfied by the full Repository.
type reviewDepsReader interface {
	GetDraftByID(ctx context.Context, tx database.Tx, tenantID, draftID string) (*Draft, error)
	GetIntimationForDraft(ctx context.Context, tx database.Tx, tenantID, intimationID string) (*IntimationContext, error)
}

// reviewWriter is the narrow write port the ReviewUseCase needs.
type reviewWriter interface {
	InsertReview(ctx context.Context, tx database.Tx, r *Review) (*Review, error)
	UpdateSagaState(ctx context.Context, tx database.Tx, draftID, tenantID, sagaState string, updateContent bool, content string, structured *StructuredContent) (*Draft, error)
}

// reviewSchema is the JSON Schema constraining the LLM's output for Revisar (strict mode).
// Only suggestions — no draft_content. Same suggestion shape as the old generateSchema,
// reusing rawSuggestion/rawCitation (package-private, defined in generate.go).
var reviewSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "suggestions": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "category":    { "type": "string" },
          "original":    { "type": "string" },
          "replacement": { "type": "string" },
          "problem":     { "type": "string" },
          "description": { "type": "string" },
          "citation": {
            "type": ["object", "null"],
            "properties": {
              "document_id": { "type": "string" },
              "page":        { "type": ["integer", "null"] },
              "quote":       { "type": "string" }
            },
            "required": ["document_id", "page", "quote"],
            "additionalProperties": false
          }
        },
        "required": ["category", "original", "replacement", "problem", "description", "citation"],
        "additionalProperties": false
      }
    }
  },
  "required": ["suggestions"],
  "additionalProperties": false
}`)

// reviewOutput is the LLM's structured response for Revisar.
type reviewOutput struct {
	Suggestions []rawSuggestion `json:"suggestions"`
}

// ReviewUseCase is the synchronous review use case. It is composed independently
// from GenerateUseCase (no outbox, no dedup — synchronous request/response).
type ReviewUseCase struct {
	uow      database.UnitOfWork
	reader   reviewDepsReader
	writer   reviewWriter
	gen      llm.Generator // nil → apperr Invalid "IA não configurada"
	emb      embedder      // nil → degraded (no RAG grounding)
	search   indexing.SearchDeps
	ragCache *RAGCache // nil → sem cache
	composer advisory.PromptComposer
	model    string
}

// ReviewUseCaseParams groups the construction parameters. All pointer fields may be nil.
type ReviewUseCaseParams struct {
	UoW      database.UnitOfWork
	Reader   reviewDepsReader
	Writer   reviewWriter
	Gen      llm.Generator
	Emb      embedder
	Search   indexing.SearchDeps
	RAGCache *RAGCache
	Composer advisory.PromptComposer
	Model    string // OpenRouter model slug; empty → generationModel fallback
}

// NewReviewUseCase wires the review use case.
func NewReviewUseCase(p ReviewUseCaseParams) *ReviewUseCase {
	model := p.Model
	if model == "" {
		model = generationModel
	}
	return &ReviewUseCase{
		uow:      p.UoW,
		reader:   p.Reader,
		writer:   p.Writer,
		gen:      p.Gen,
		emb:      p.Emb,
		search:   p.Search,
		ragCache: p.RAGCache,
		composer: p.Composer,
		model:    model,
	}
}

// ReviewDraft implements POST /v1/pecas/:id/review.
//
// Phase 1 (READ, short tx): load draft + guard content + guard saga.
// Phase 2 (LLM, NO tx):    RAG → compose → generate → buildFindings.
// Phase 3 (WRITE, short tx): InsertReview + UpdateSagaState → REVIEWED.
//
// On LLM failure the draft stays in its current state — no FAILED is written.
func (uc *ReviewUseCase) ReviewDraft(ctx context.Context, cmd ReviewDraftCommand) (*ReviewResult, error) {
	if uc.gen == nil {
		return nil, apperr.NewInvalid("IA não configurada: OPENROUTER_API_KEY não definida")
	}

	// ── Phase 1: READ (short tx) ──────────────────────────────────────────────
	var d *Draft
	var crid *string // court_record_id resolved from the intimation (nil → whole-tenant)
	if err := uc.uow.Do(ctx, cmd.TenantID, func(tx database.Tx) error {
		loaded, err := uc.reader.GetDraftByID(ctx, tx, cmd.TenantID, cmd.DraftID)
		if err != nil {
			return err
		}
		d = loaded

		// Guard: empty content → advogado has nothing to review yet.
		if d.Content == "" {
			return apperr.NewInvalid("sem conteúdo para revisar: gere a minuta antes de revisar")
		}
		// Guard: generation in flight → block concurrent Revisar on an unstable draft.
		if d.SagaState == SagaStateExtracting {
			return apperr.NewConflict("geração em andamento: aguarde antes de revisar")
		}

		// Resolve court_record_id for intimation-scoped RAG (non-fatal).
		if d.IntimationID != "" {
			intimation, e := uc.reader.GetIntimationForDraft(ctx, tx, cmd.TenantID, d.IntimationID)
			if e != nil {
				slog.WarnContext(ctx, "review: intimation load failed — falling back to tenant-wide RAG",
					slog.String("draft_id", cmd.DraftID),
					slog.Any("error", e),
				)
			} else if intimation != nil && intimation.CourtRecordID != "" {
				crid = &intimation.CourtRecordID
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}

	// ── Phase 2: LLM (NO tx — connection released) ───────────────────────────

	// 2a. RAG: embed draft content → search chunks scoped to intimation's court_record.
	chunks, _, grounded := runRAG(ctx, uc.emb, uc.search, uc.ragCache, cmd.TenantID, crid, d.Content, 8)

	// 2b. Compose review prompt.
	reviewCtx := advisory.ReviewContext{
		Content:   d.Content,
		PieceType: d.PieceType,
		Chunks:    chunks,
	}
	composed, err := uc.composer.ComposeReview(advisory.AgentReviewMinuta, reviewCtx)
	if err != nil {
		return nil, fmt.Errorf("review: compose prompt: %w", err)
	}

	// 2c. Call LLM.
	rawBytes, err := uc.gen.GenerateJSON(ctx, llm.Request{
		System:     composed.System,
		User:       composed.User,
		Schema:     reviewSchema,
		SchemaName: "review_minuta",
		Model:      uc.model,
		MaxTokens:  4096,
		UseCase:    "draft.review",
		TenantID:   cmd.TenantID,
	})
	if err != nil {
		// Return the error typed as-is; the handler maps via WriteError.
		// Do NOT persist FAILED — the draft stays in its current state.
		return nil, fmt.Errorf("review: llm call: %w", err)
	}

	var out reviewOutput
	if err := json.Unmarshal(rawBytes, &out); err != nil {
		return nil, fmt.Errorf("review: parse llm output: %w", err)
	}

	// 2d. Validate and filter suggestions against the CURRENT draft content.
	findings, coverage := buildFindings(out.Suggestions, grounded, len(chunks), d.Content)

	// ── Phase 3: WRITE (short tx) ─────────────────────────────────────────────
	var result *ReviewResult
	if err := uc.uow.Do(ctx, cmd.TenantID, func(tx database.Tx) error {
		rev, e := uc.writer.InsertReview(ctx, tx, &Review{
			DraftID:      cmd.DraftID,
			Findings:     findings,
			Coverage:     coverage,
			ModelVersion: uc.model,
			RulesVersion: composed.PromptVersion,
			Status:       ReviewStatusCompleted,
			GeneratedAt:  time.Now(),
		})
		if e != nil {
			return fmt.Errorf("review: insert review: %w", e)
		}

		updated, e := uc.writer.UpdateSagaState(ctx, tx, cmd.DraftID, cmd.TenantID, SagaStateReviewed, false, "", nil)
		if e != nil {
			return fmt.Errorf("review: update saga state: %w", e)
		}

		result = &ReviewResult{Review: rev, SagaState: updated.SagaState}
		return nil
	}); err != nil {
		return nil, err
	}

	slog.InfoContext(ctx, "draft review completed",
		slog.String("draft_id", cmd.DraftID),
		slog.Int("findings", len(findings)),
		slog.Bool("grounded", grounded),
	)
	return result, nil
}

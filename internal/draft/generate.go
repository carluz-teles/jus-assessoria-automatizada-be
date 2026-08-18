package draft

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jusassessoria/platform/internal/advisory"
	"github.com/jusassessoria/platform/internal/indexing"
	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
	"github.com/jusassessoria/platform/lib/llm"
)

// outboxPublisher is the narrow port the generation use case needs from the outbox.
// Satisfied by *events.Outbox (production) and a fakeOutbox in tests.
type outboxPublisher interface {
	Publish(ctx context.Context, tx database.Tx, ev events.Event) error
}

// generateDeduper is the narrow dedup port for the generation use case. It marks
// (consumer, eventID) within the caller's tx and reports whether it was already there.
// Satisfied by the txDeduper adapter (same pattern as internal/indexing).
type generateDeduper interface {
	SeenOrMark(ctx context.Context, tx database.Tx, consumer, eventID string) (seen bool, err error)
}

// txGenerateDeduper adapts lib/events.Dedup to the generateDeduper port.
type txGenerateDeduper struct{}

// NewGenerateDeduper returns the generateDeduper the generation use case uses.
func NewGenerateDeduper() generateDeduper { return txGenerateDeduper{} }

func (txGenerateDeduper) SeenOrMark(ctx context.Context, tx database.Tx, consumer, eventID string) (bool, error) {
	return events.NewDedup(tx).SeenOrMark(ctx, consumer, eventID)
}

// generate.go is the async AI generation use case (Peticionamento Fatia 3). It is the
// worker-ai counterpart of handler.go's POST /generate trigger: the handler publishes
// draft.generation_requested into the outbox; the worker-ai listener calls
// OnGenerationRequested here. The same Unit-of-Work / processed_event / outbox pattern
// as internal/deadline's listener (the closest analog), but the write path is richer
// (RAG + LLM + finding validation + coverage computation).

// generationConsumer is the processed_event consumer name for the generation listener.
// Dedup is per-consumer (docs §4c.3), so it is slice+job-specific.
const generationConsumer = "draft_ai"

// generationModel is the single source of truth for the Claude model used in the
// generation pipeline. Referenced in the llm.Request (prompt call) and stamped on
// the review row (model_version) so findings are always attributable to the model
// that produced them. Bump here when the model changes — one place, not two.
const generationModel = "claude-opus-4-8"

// generationDepsReader is the narrow read port the generation use case needs to load
// case context for composing the advisory prompt. Satisfied by the read Repository
// (GetDraftByID + optionally GetIntimationForDraft), but kept as a port so the unit
// tests can inject a fake without a full repo mock.
type generationDepsReader interface {
	GetDraftByID(ctx context.Context, tx database.Tx, tenantID, draftID string) (*Draft, error)
	GetIntimationForDraft(ctx context.Context, tx database.Tx, tenantID, intimationID string) (*IntimationContext, error)
}

// generationWriter is the narrow write port the generation use case needs. A separate
// interface (not the full Repository) keeps the fake in tests minimal.
type generationWriter interface {
	UpdateSagaState(ctx context.Context, tx database.Tx, draftID, tenantID, sagaState string, updateContent bool, content string) (*Draft, error)
	InsertReview(ctx context.Context, tx database.Tx, r *Review) (*Review, error)
}

// embedder is the narrow embedding port for RAG. Satisfied by *indexing.VoyageEmbedder
// or a test fake. nil → degraded path (no grounding).
type embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, string, error)
}

// VoyageEmbedder is the exported type alias so cmd/worker-ai can use *indexing.VoyageEmbedder
// as the embedder port without an import cycle. The concrete type satisfies this interface.
type VoyageEmbedder = embedder

// GenerateUseCase is the async handler for draft.generation_requested events. It holds
// all its ports as interfaces so tests inject fakes — the real LLM/embedder APIs are
// NEVER called under test.
type GenerateUseCase struct {
	uow      database.UnitOfWork
	reader   generationDepsReader
	writer   generationWriter
	outbox   outboxPublisher
	dedup    generateDeduper
	gen      llm.Generator       // nil → FAILED "IA não configurada"
	emb      embedder            // nil → degraded (no grounding)
	search   indexing.SearchDeps // Pool may be nil → degraded
	composer advisory.PromptComposer
	model    string // OpenRouter model slug (from config); falls back to generationModel
	now      func() time.Time
}

// GenerateUseCaseParams groups the construction parameters for GenerateUseCase. All
// pointer/interface fields may be nil (degraded path for optional deps).
type GenerateUseCaseParams struct {
	UoW      database.UnitOfWork
	Reader   generationDepsReader
	Writer   generationWriter
	Outbox   outboxPublisher
	Dedup    generateDeduper
	Gen      llm.Generator
	Emb      embedder
	Search   indexing.SearchDeps
	Composer advisory.PromptComposer
	Model    string           // OpenRouter model slug; empty → generationModel fallback
	Now      func() time.Time // defaults to time.Now
}

// NewGenerateUseCase wires the generation use case. All optional deps (Gen, Emb, Search)
// may be nil — the use case handles the degraded paths.
func NewGenerateUseCase(p GenerateUseCaseParams) *GenerateUseCase {
	if p.Now == nil {
		p.Now = time.Now
	}
	if p.Model == "" {
		p.Model = generationModel
	}
	return &GenerateUseCase{
		uow:      p.UoW,
		reader:   p.Reader,
		writer:   p.Writer,
		outbox:   p.Outbox,
		dedup:    p.Dedup,
		gen:      p.Gen,
		emb:      p.Emb,
		search:   p.Search,
		composer: p.Composer,
		model:    p.Model,
		now:      p.Now,
	}
}

// generatedOutput is the LLM's structured response for one generation call.
type generatedOutput struct {
	DraftContent string          `json:"draft_content"`
	Suggestions  []rawSuggestion `json:"suggestions"`
}

// rawSuggestion is one LLM-suggested improvement before validation.
type rawSuggestion struct {
	Category    string       `json:"category"`
	Original    string       `json:"original"`
	Replacement string       `json:"replacement"`
	Problem     string       `json:"problem"`
	Description string       `json:"description"`
	Citation    *rawCitation `json:"citation,omitempty"`
}

// rawCitation is the LLM's citation before validation.
type rawCitation struct {
	DocumentID string `json:"document_id"`
	Page       int    `json:"page"`
	Quote      string `json:"quote"`
}

// generateSchema is the JSON Schema constraining the LLM's output via
// structured output (strict). draft_content + suggestions array; citation is optional
// per suggestion. The schema lives here (the caller) per the design decision.
var generateSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "draft_content": { "type": "string" },
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
  "required": ["draft_content", "suggestions"],
  "additionalProperties": false
}`)

// OnGenerationRequested is the async entry point called by the worker-ai listener.
// It:
//  1. Dedups (processed_event guard — consumer "draft_ai").
//  2. Reloads the draft; guards saga_state == EXTRACTING (skip if obsolete).
//  3. Embeds the intimation text and runs RAG (degraded if embedder nil or empty result).
//  4. Calls the LLM to generate draft_content + suggestions.
//  5. Validates and filters suggestions (substring match + citation requirement).
//  6. Persists: UPDATE draft SET content=..., saga_state=REVIEWED; INSERT review;
//     mark processed_event; outbox.Publish(review.completed) — all in ONE tx.
//
// On any terminal failure (LLM nil, parse error, context timeout after retries):
// persists saga_state=FAILED + review(status=FAILED) in a short tx.
//
// Transient failures (infra/unavailable) are returned unwrapped for asynq retry.
func (uc *GenerateUseCase) OnGenerationRequested(ctx context.Context, ev GenerationRequested) error {
	// ── 1. Generator nil check: FAILED immediately, skip retry ───────────────
	if uc.gen == nil {
		return uc.persistFailure(ctx, ev.TenantID, ev.DraftID, ev.EventID, "IA não configurada")
	}

	// ── 2. Dedup happens in the effect tx (step 7 / persistFailure), NOT here ──
	// The mark MUST commit atomically with the writes it guards (at-least-once
	// contract, erd-backend §4c.3): a separate dedup tx would leave the draft
	// stuck in EXTRACTING forever if the process crashed between the dedup commit
	// and the effect commit. The saga==EXTRACTING guard (step 3) already short-
	// circuits redeliveries that arrive after a terminal state, so the LLM call
	// is only wasted on a genuine concurrent in-flight duplicate (rare).

	// ── 3. Read draft + guard saga_state == EXTRACTING ────────────────────────
	var draft *Draft
	var intimation *IntimationContext
	if err := uc.uow.Do(ctx, ev.TenantID, func(tx database.Tx) error {
		d, e := uc.reader.GetDraftByID(ctx, tx, ev.TenantID, ev.DraftID)
		if e != nil {
			return e
		}
		draft = d
		if d.SagaState != SagaStateExtracting {
			// Obsolete event (re-trigger while already REVIEWED/FAILED/CREATED).
			// Treat as SkipRetry by returning a sentinel; the listener wraps it.
			return errObsoleteSaga
		}
		// Load intimation context for prompt composition (optional — blank/processo drafts have none).
		if d.IntimationID != "" {
			i, e2 := uc.reader.GetIntimationForDraft(ctx, tx, ev.TenantID, d.IntimationID)
			if e2 != nil {
				// Non-fatal: compose without intimation context (degraded).
				slog.WarnContext(ctx, "draft generate: intimation load failed",
					slog.String("draft_id", ev.DraftID), slog.Any("error", e2))
			} else {
				intimation = i
			}
		}
		return nil
	}); err != nil {
		if isErrObsoleteSaga(err) {
			return fmt.Errorf("%w: %w", err, errSkipRetry)
		}
		return fmt.Errorf("draft generate: load draft: %w", err)
	}

	// ── 4. RAG: embed + search chunks ─────────────────────────────────────────
	queryText := buildQueryText(draft, intimation)
	chunks, grounded := uc.runRAG(ctx, ev.TenantID, draft, queryText)

	// ── 5. Compose prompt and call LLM ────────────────────────────────────────
	draftCtx := buildDraftContext(draft, intimation, chunks)
	composed, err2 := uc.composer.ComposeDraft(advisory.AgentDraftMinuta, draftCtx)
	if err2 != nil {
		return uc.persistFailure(ctx, ev.TenantID, ev.DraftID, ev.EventID, fmt.Sprintf("compose prompt: %v", err2))
	}

	rawBytes, err3 := uc.gen.GenerateJSON(ctx, llm.Request{
		System:     composed.System,
		User:       composed.User,
		Schema:     generateSchema,
		SchemaName: "draft_minuta",
		Model:      uc.model,
		MaxTokens:  4096,
	})
	if err3 != nil {
		// Transient LLM errors stay retryable; terminal (bad key / parse) become FAILED.
		if isTerminalGenErr(err3) {
			return uc.persistFailure(ctx, ev.TenantID, ev.DraftID, ev.EventID, fmt.Sprintf("llm: %v", err3))
		}
		return fmt.Errorf("draft generate: llm call: %w", err3)
	}

	var out generatedOutput
	if err4 := json.Unmarshal(rawBytes, &out); err4 != nil {
		return uc.persistFailure(ctx, ev.TenantID, ev.DraftID, ev.EventID, fmt.Sprintf("parse llm output: %v", err4))
	}

	// ── 6. Validate and filter suggestions ────────────────────────────────────
	findings, coverage := buildFindings(out, grounded, len(chunks))

	// ── 7. Persist success in ONE tx ──────────────────────────────────────────
	var reviewID string
	if err5 := uc.uow.Do(ctx, ev.TenantID, func(tx database.Tx) error {
		// Dedup in the SAME tx as the effect: a concurrent delivery that already
		// marked this event skips the writes (only one review is persisted).
		if seen, e := uc.dedup.SeenOrMark(ctx, tx, generationConsumer, ev.EventID); e != nil {
			return e
		} else if seen {
			return nil
		}
		if _, e := uc.writer.UpdateSagaState(ctx, tx, ev.DraftID, ev.TenantID, SagaStateReviewed, true, out.DraftContent); e != nil {
			return fmt.Errorf("update saga state: %w", e)
		}

		rev, e := uc.writer.InsertReview(ctx, tx, &Review{
			DraftID:      ev.DraftID,
			Findings:     findings,
			Coverage:     coverage,
			ModelVersion: uc.model,
			RulesVersion: composed.PromptVersion,
			Status:       ReviewStatusCompleted,
			GeneratedAt:  uc.now(),
		})
		if e != nil {
			return fmt.Errorf("insert review: %w", e)
		}
		reviewID = rev.ID

		ev2 := newReviewCompleted(ev.DraftID, rev.ID, ReviewStatusCompleted)
		if e := uc.outbox.Publish(ctx, tx, ev2); e != nil {
			return fmt.Errorf("publish review.completed: %w", e)
		}
		return nil
	}); err5 != nil {
		return fmt.Errorf("draft generate: persist success: %w", err5)
	}

	slog.InfoContext(ctx, "draft generation completed",
		slog.String("draft_id", ev.DraftID),
		slog.String("review_id", reviewID),
		slog.Int("findings", len(findings)),
		slog.Bool("grounded", grounded),
	)
	return nil
}

// persistFailure writes saga_state=FAILED + review(status=FAILED) in a short tx.
// Called for terminal conditions (nil generator, parse error, LLM auth failure).
// It returns a SkipRetry-wrapped error so the listener archives the task.
func (uc *GenerateUseCase) persistFailure(ctx context.Context, tenantID, draftID, eventID, reason string) error {
	err := uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		// Dedup in the same tx as the FAILED effect (skip if already processed).
		if eventID != "" {
			if seen, e := uc.dedup.SeenOrMark(ctx, tx, generationConsumer, eventID); e != nil {
				return e
			} else if seen {
				return nil
			}
		}
		if _, e := uc.writer.UpdateSagaState(ctx, tx, draftID, tenantID, SagaStateFailed, false, ""); e != nil {
			return e
		}
		_, e := uc.writer.InsertReview(ctx, tx, &Review{
			DraftID:      draftID,
			Findings:     []Finding{},
			Coverage:     Coverage{DocumentsCited: []string{}, Error: reason},
			ModelVersion: "",
			RulesVersion: "",
			Status:       ReviewStatusFailed,
			GeneratedAt:  uc.now(),
		})
		return e
	})
	if err != nil {
		// If even the failure persist fails (e.g. draft deleted), still SkipRetry.
		slog.ErrorContext(ctx, "draft generate: persist failure failed",
			slog.String("draft_id", draftID), slog.Any("error", err))
	}
	return fmt.Errorf("draft generation failed: %s: %w", reason, errSkipRetry)
}

// runRAG embeds the query text and searches for the top-8 chunks. Returns the
// chunk texts and whether grounding was achieved. Degrades gracefully when the
// embedder is nil, the search pool is nil, or the search returns nothing.
func (uc *GenerateUseCase) runRAG(ctx context.Context, tenantID string, d *Draft, queryText string) (chunks []string, grounded bool) {
	if uc.emb == nil || uc.search.Pool == nil {
		return []string{}, false
	}
	vecs, _, err := uc.emb.Embed(ctx, []string{queryText})
	if err != nil || len(vecs) == 0 {
		slog.WarnContext(ctx, "draft generate: embed failed",
			slog.String("draft_id", d.ID), slog.Any("error", err))
		return []string{}, false
	}

	var crid *string
	if d.CaseID != "" {
		// scope to this case's court record documents — requires the court_record_id.
		// The draft entity only carries case_id; we pass nil to search tenant-wide.
		// A future improvement: join court_record to resolve the id here.
	}

	hits, err := indexing.SearchChunks(ctx, uc.search, tenantID, crid, vecs[0], 8)
	if err != nil {
		slog.WarnContext(ctx, "draft generate: search chunks failed",
			slog.String("draft_id", d.ID), slog.Any("error", err))
		return []string{}, false
	}
	if len(hits) == 0 {
		return []string{}, false
	}

	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.Text
	}
	return out, true
}

// buildQueryText builds the RAG query string from the draft and intimation context.
func buildQueryText(d *Draft, i *IntimationContext) string {
	parts := make([]string, 0, 3)
	parts = append(parts, d.PieceType)
	if i != nil {
		if i.Type != "" {
			parts = append(parts, i.Type)
		}
	}
	return strings.Join(parts, " ")
}

// buildDraftContext converts the loaded domain objects to the advisory DraftContext.
func buildDraftContext(d *Draft, i *IntimationContext, chunks []string) advisory.DraftContext {
	dc := advisory.DraftContext{
		PieceType: d.PieceType,
		Chunks:    chunks,
	}
	if i != nil {
		dc.IntimationType = i.Type
	}
	return dc
}

// buildFindings validates and filters the LLM's raw suggestions:
//   - original must appear as a substring in draft_content → drop if not
//   - Argumento/Coerência must have a non-empty citation → drop if absent
//   - cap at 10 after filtering
//
// Returns the validated Finding slice and the Coverage summary.
func buildFindings(out generatedOutput, grounded bool, chunksUsed int) ([]Finding, Coverage) {
	total := len(out.Suggestions)
	documentsCited := map[string]bool{}

	findings := make([]Finding, 0, min(total, 10))
	for _, s := range out.Suggestions {
		if len(findings) >= 10 {
			break // top-10 cap; the remainder is counted as dropped below
		}
		// Substring validation: original must appear in draft_content.
		if !strings.Contains(out.DraftContent, s.Original) {
			continue
		}
		// Citation requirement for Argumento and Coerência.
		if citationRequired(s.Category) {
			if s.Citation == nil || s.Citation.DocumentID == "" {
				continue
			}
		}

		f := Finding{
			N:           len(findings) + 1,
			Category:    s.Category,
			Original:    s.Original,
			Replacement: s.Replacement,
			Problem:     s.Problem,
			Description: s.Description,
		}
		if s.Citation != nil && s.Citation.DocumentID != "" {
			f.Citation = &Citation{
				DocumentID: s.Citation.DocumentID,
				Page:       s.Citation.Page,
				Quote:      s.Citation.Quote,
			}
			documentsCited[s.Citation.DocumentID] = true
		}
		findings = append(findings, f)
	}
	// Everything not kept (substring/citation failures + top-10 cap overflow) is
	// a drop — total minus the validated findings.
	dropped := total - len(findings)

	cited := make([]string, 0, len(documentsCited))
	for id := range documentsCited {
		cited = append(cited, id)
	}

	coverage := Coverage{
		Grounded:           grounded,
		ChunksUsed:         chunksUsed,
		SuggestionsTotal:   total,
		SuggestionsDropped: dropped,
		DocumentsCited:     cited,
	}
	if coverage.DocumentsCited == nil {
		coverage.DocumentsCited = []string{}
	}
	return findings, coverage
}

// isTerminalGenErr reports whether the LLM error is terminal (invalid key, bad request)
// vs transient (rate limit, timeout). Terminal → FAILED; transient → retry.
func isTerminalGenErr(err error) bool {
	ae, ok := apperr.From(err)
	if !ok {
		return false
	}
	return ae.Kind == apperr.KindInvalid || ae.Kind == apperr.KindUnauthorized
}

// min returns the smaller of two ints.
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// errObsoleteSaga is a sentinel returned when the draft's saga_state is not EXTRACTING
// at consumer time (the event is obsolete — the draft was already re-triggered or
// completed by another delivery).
var errObsoleteSaga = apperr.NewInvalid("generation saga: draft is not in EXTRACTING state")

// errSkipRetry is the terminal sentinel the listener wraps with asynq.SkipRetry.
// Keeping it here (not in the listener) keeps the use case asynq-free while still
// letting the listener classify the outcome correctly.
var errSkipRetry = apperr.NewInvalid("generation terminal: skip retry")

func isErrObsoleteSaga(err error) bool {
	return err == errObsoleteSaga
}

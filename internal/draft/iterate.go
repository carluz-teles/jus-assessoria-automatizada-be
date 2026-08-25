package draft

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jusassessoria/platform/internal/advisory"
	"github.com/jusassessoria/platform/internal/indexing"
	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/llm"
)

// iterate.go is the Peça v2 synchronous iteration endpoint (POST
// /v1/pecas/:id/iterate). Given the current structured draft + a scope
// (whole ou a section) + a kind (quick adjust) OR an instruction (livre),
// calls the LLM once and returns 1..N section changes with
// {category, explanation, new_paragraphs} — the FE renders each as a
// "Ajuste proposto" card the advogado accepts or discards individually.
//
// Synchronous by design: iterations are pointual (a few seconds) and the FE
// needs the diff on-screen immediately. If we ever need to batch/async
// (very long peças, very complex prompts), we split later — the wire shape
// stays the same.

// ── Request / Response shapes ────────────────────────────────────────────────

// IterateCommand is the input the use case receives (edge params + advogado body).
type IterateCommand struct {
	TenantID    string
	DraftID     string
	Scope       IterateScope
	Kind        string // "" when the advogado sent a free instruction instead
	Instruction string // "" when the advogado clicked a quick adjust chip
}

// IterateScope is a copy of advisory.IterateScope but lives with the entity so
// callers of the draft slice don't have to import advisory.
type IterateScope struct {
	Kind      string // "whole" | "section"
	SectionID string // set when Kind == "section"
}

// IterateResult is the response: a list of changes (0..N). Empty when the LLM
// found nothing worth changing — the FE shows a toast and stays put.
type IterateResult struct {
	Changes []SectionChange `json:"changes"`
}

// SectionChange is one card in the FE's "Ajuste proposto" panel. category and
// explanation come from the LLM; old_paragraphs is filled from the current
// draft state (never trust the LLM to echo it back).
type SectionChange struct {
	SectionID     string   `json:"section_id"`
	SectionRoman  string   `json:"section_roman"`
	SectionTitle  string   `json:"section_title"`
	Category      string   `json:"category"`
	Explanation   string   `json:"explanation"`
	OldParagraphs []string `json:"old_paragraphs"`
	NewParagraphs []string `json:"new_paragraphs"`
}

// ── Constants ────────────────────────────────────────────────────────────────

// validQuickAdjustKinds is the closed set of quick-adjust "kinds" the FE
// sends. Empty (free instruction) is also valid — enforced at Validate.
var validQuickAdjustKinds = map[string]struct{}{
	"concise":          {},
	"emphatic":         {},
	"reinforce_thesis": {},
	"add_grounds":      {},
}

// validCategories is the closed set of `changes[].category` values the LLM
// may return. Enforced post-response; anything else defaults to "AJUSTE".
var validCategories = map[string]struct{}{
	"CLAREZA":       {},
	"CONCISÃO":      {},
	"ÊNFASE":        {},
	"FUNDAMENTAÇÃO": {},
	"COMPLETUDE":    {},
	"COERÊNCIA":     {},
	"AJUSTE":        {},
}

// iterateSchema is the JSON Schema constraining the LLM output (strict mode).
// section_id must match one of the section IDs injected in the user prompt;
// the caller re-checks after unmarshal (schema alone can't enforce a dynamic
// enum).
var iterateSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "changes": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "section_id":     { "type": "string" },
          "category":       { "type": "string" },
          "explanation":    { "type": "string" },
          "new_paragraphs": {
            "type": "array",
            "items": { "type": "string" }
          }
        },
        "required": ["section_id", "category", "explanation", "new_paragraphs"],
        "additionalProperties": false
      }
    }
  },
  "required": ["changes"],
  "additionalProperties": false
}`)

// rawIterateOutput is the LLM's raw response before validation. section_id and
// category are re-checked post-parse against the current draft / closed sets.
type rawIterateOutput struct {
	Changes []rawSectionChange `json:"changes"`
}

type rawSectionChange struct {
	SectionID     string   `json:"section_id"`
	Category      string   `json:"category"`
	Explanation   string   `json:"explanation"`
	NewParagraphs []string `json:"new_paragraphs"`
}

// ── Use case ────────────────────────────────────────────────────────────────

// iterateReader is the narrow read port the IterateUseCase depends on. Same
// shape as generationDepsReader (draft + intimation + parties), but declared
// separately so tests can inject a distinct fake for iterate scenarios.
type iterateReader interface {
	GetDraftByID(ctx context.Context, tx database.Tx, tenantID, draftID string) (*Draft, error)
	GetIntimationForDraft(ctx context.Context, tx database.Tx, tenantID, intimationID string) (*IntimationContext, error)
	GetPartiesForDraft(ctx context.Context, tx database.Tx, tenantID, caseID string) ([]PartyInfo, error)
}

// IterateUseCase handles POST /v1/pecas/:id/iterate synchronously. Holds all
// LLM-side ports as interfaces so tests inject fakes (the real LLM is NEVER
// called under test — same guard as Gerar/Revisar).
type IterateUseCase struct {
	uow      database.UnitOfWork
	reader   iterateReader
	composer advisory.PromptComposer
	gen      llm.Generator
	model    string
	emb      embedder            // optional — RAG fallback (degraded when nil)
	search   indexing.SearchDeps // optional — RAG fallback (degraded when nil)
	ragCache *RAGCache           // nil → sem cache
}

// IterateParams is the DI bundle for NewIterateUseCase.
type IterateParams struct {
	UoW      database.UnitOfWork
	Reader   iterateReader
	Composer advisory.PromptComposer
	Gen      llm.Generator
	Model    string
	Embedder embedder
	Search   indexing.SearchDeps
	RAGCache *RAGCache
}

// NewIterateUseCase constructs the use case with the injected ports.
func NewIterateUseCase(p IterateParams) *IterateUseCase {
	return &IterateUseCase{
		uow:      p.UoW,
		reader:   p.Reader,
		composer: p.Composer,
		gen:      p.Gen,
		model:    p.Model,
		emb:      p.Embedder,
		search:   p.Search,
		ragCache: p.RAGCache,
	}
}

// Iterate is the synchronous LLM call. Returns IterateResult with 0..N changes.
// The caller (handler) serves it as JSON; the FE draws one card per change.
//
// Guards:
//   - gen nil → 422 (ErrIANotConfigured)
//   - draft not found / foreign tenant → 404 (ErrDraftNotFound)
//   - structured_content nil AND parser can't find sections → 422 (peça vazia)
//   - LLM terminal error (bad key / invalid JSON) → 502 with reason
//   - transient LLM error → propagated (caller may retry)
func (uc *IterateUseCase) Iterate(ctx context.Context, cmd IterateCommand) (*IterateResult, error) {
	if uc.gen == nil {
		return nil, apperr.NewInvalid("IA não configurada — iteração indisponível.")
	}

	// ── Load draft + optional intimation + parties in one tenant-scoped tx ──
	var draft *Draft
	var intimation *IntimationContext
	var parties []PartyInfo
	err := uc.uow.Do(ctx, cmd.TenantID, func(tx database.Tx) error {
		d, e := uc.reader.GetDraftByID(ctx, tx, cmd.TenantID, cmd.DraftID)
		if e != nil {
			return e
		}
		draft = d
		if d.IntimationID != "" {
			i, e2 := uc.reader.GetIntimationForDraft(ctx, tx, cmd.TenantID, d.IntimationID)
			if e2 == nil {
				intimation = i
			}
		}
		if d.CaseID != "" {
			pp, e3 := uc.reader.GetPartiesForDraft(ctx, tx, cmd.TenantID, d.CaseID)
			if e3 == nil {
				parties = pp
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Resolve the structured shape — fall back to the parser when the column
	// is still NULL (a draft not yet touched by Fatia B or by a lazy write-back).
	structured := draft.StructuredContent
	if structured == nil {
		structured = ParseStructured(draft.Content)
	}
	if structured == nil || len(structured.Sections) == 0 {
		return nil, apperr.NewInvalid(
			"Peça sem seções para iterar. Gere ou escreva a peça antes de pedir ajustes.")
	}

	// Validate scope.section_id against the actual sections (fail fast).
	if cmd.Scope.Kind == "section" {
		found := false
		for _, s := range structured.Sections {
			if s.ID == cmd.Scope.SectionID {
				found = true
				break
			}
		}
		if !found {
			return nil, apperr.NewInvalid(
				"scope.section_id não existe na peça: " + cmd.Scope.SectionID)
		}
	}

	// ── RAG (best-effort) ────────────────────────────────────────────────────
	var crid *string
	if intimation != nil && intimation.CourtRecordID != "" {
		crid = &intimation.CourtRecordID
	}
	queryText := buildIterateQuery(cmd, structured)
	chunks, _, _ := runRAG(ctx, uc.emb, uc.search, uc.ragCache, cmd.TenantID, crid, queryText, 6)

	// ── Compose prompt + call LLM ────────────────────────────────────────────
	iterCtx := buildIterateContext(draft, intimation, parties, structured, cmd, chunks)
	composed, err := uc.composer.ComposeIterate(advisory.AgentDraftIterate, iterCtx)
	if err != nil {
		return nil, fmt.Errorf("iterate: compose prompt: %w", err)
	}

	rawBytes, err := uc.gen.GenerateJSON(ctx, llm.Request{
		System:     composed.System,
		User:       composed.User,
		Schema:     iterateSchema,
		SchemaName: "draft_iterate",
		Model:      uc.model,
		MaxTokens:  4096,
	})
	if err != nil {
		if isTerminalGenErr(err) {
			return nil, apperr.NewInvalid("Falha na iteração pela IA: " + err.Error())
		}
		return nil, fmt.Errorf("iterate: llm call: %w", err)
	}

	var raw rawIterateOutput
	if err := json.Unmarshal(rawBytes, &raw); err != nil {
		return nil, apperr.NewInvalid("Resposta da IA em formato inesperado.")
	}

	// ── Post-validate + hydrate ──────────────────────────────────────────────
	changes := hydrateChanges(raw.Changes, structured, cmd.Scope)

	slog.InfoContext(ctx, "draft iterate completed",
		slog.String("draft_id", cmd.DraftID),
		slog.String("scope", cmd.Scope.Kind),
		slog.String("kind", cmd.Kind),
		slog.Int("raw_changes", len(raw.Changes)),
		slog.Int("kept_changes", len(changes)),
	)

	return &IterateResult{Changes: changes}, nil
}

// hydrateChanges keeps only changes whose section_id exists in the current
// draft (drops LLM hallucinations); when scope=section, it also filters to
// just that section. Fills old_paragraphs + roman + title from the state,
// normalizes category to the closed set (falls back to "AJUSTE").
func hydrateChanges(raw []rawSectionChange, structured *StructuredContent, scope IterateScope) []SectionChange {
	// Index sections by id for O(1) lookup.
	byID := make(map[string]StructuredSection, len(structured.Sections))
	for _, s := range structured.Sections {
		byID[s.ID] = s
	}

	out := make([]SectionChange, 0, len(raw))
	for _, r := range raw {
		s, ok := byID[r.SectionID]
		if !ok {
			continue // ignore unknown sections (LLM hallucinated an id)
		}
		if scope.Kind == "section" && r.SectionID != scope.SectionID {
			continue // scope=section: ignore anything outside the requested one
		}
		// Skip no-op changes (LLM echoed the same paragraphs).
		if sameParagraphs(s.Paragraphs, r.NewParagraphs) {
			continue
		}
		out = append(out, SectionChange{
			SectionID:     s.ID,
			SectionRoman:  s.Roman,
			SectionTitle:  s.Title,
			Category:      normalizeCategory(r.Category),
			Explanation:   r.Explanation,
			OldParagraphs: append([]string{}, s.Paragraphs...), // defensive copy
			NewParagraphs: append([]string{}, r.NewParagraphs...),
		})
	}
	return out
}

// sameParagraphs returns true when both slices carry the same trimmed content.
func sameParagraphs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// normalizeCategory forces the LLM's category into the closed set. Unknown
// values → "AJUSTE" (the safe default the FE knows how to render).
func normalizeCategory(cat string) string {
	if _, ok := validCategories[cat]; ok {
		return cat
	}
	return "AJUSTE"
}

// buildIterateContext maps the domain entities into the composer's context.
func buildIterateContext(draft *Draft, intimation *IntimationContext, parties []PartyInfo, structured *StructuredContent, cmd IterateCommand, chunks []string) advisory.IterateContext {
	ctx := advisory.IterateContext{
		PieceType:   draft.PieceType,
		Preamble:    structured.Preamble.Paragraphs,
		Scope:       advisory.IterateScope{Kind: cmd.Scope.Kind, SectionID: cmd.Scope.SectionID},
		Kind:        cmd.Kind,
		Instruction: cmd.Instruction,
		Chunks:      chunks,
	}
	// Sections snapshot.
	ctx.Sections = make([]advisory.IterateSection, 0, len(structured.Sections))
	for _, s := range structured.Sections {
		ctx.Sections = append(ctx.Sections, advisory.IterateSection{
			ID:         s.ID,
			Roman:      s.Roman,
			Title:      s.Title,
			Paragraphs: s.Paragraphs,
		})
	}
	// Process metadata.
	if intimation != nil {
		ctx.Court = intimation.Court
		ctx.Degree = intimation.Degree
		ctx.Class = intimation.Class
		ctx.Subject = intimation.Subject
		ctx.CNJNumber = intimation.CNJNumber
		ctx.JudgingBody = intimation.JudgingBody
	}
	// Parties → PartyCtx (name + role + first counsel label).
	ctx.Parties = make([]advisory.PartyCtx, 0, len(parties))
	for _, p := range parties {
		ctx.Parties = append(ctx.Parties, advisory.PartyCtx{
			Role:    p.Role,
			Name:    p.Name,
			Counsel: p.Counsel,
		})
	}
	return ctx
}

// buildIterateQuery is the seed string for the RAG search — the advogado's
// instruction (if any) or the kind label + a snippet of the target section.
// Keeps chunks relevant to what's being asked; degraded when embedder is nil.
func buildIterateQuery(cmd IterateCommand, structured *StructuredContent) string {
	seed := cmd.Instruction
	if seed == "" {
		seed = cmd.Kind
	}
	if cmd.Scope.Kind == "section" {
		for _, s := range structured.Sections {
			if s.ID == cmd.Scope.SectionID && len(s.Paragraphs) > 0 {
				seed += " " + s.Paragraphs[0]
				break
			}
		}
	}
	return seed
}

// Ensure iterateReader is satisfied by the full Repository interface — cheap
// compile-time guarantee that wiring in cmd/api pumps a real repo in without
// a wrapper.
var _ iterateReader = (Repository)(nil)

// Sentinel: keep errors import used even when the file has no direct usage.
var _ = errors.Is

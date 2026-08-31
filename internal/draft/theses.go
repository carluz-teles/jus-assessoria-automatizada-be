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
	"strings"
	"sync"
	"time"

	"github.com/jusassessoria/platform/internal/advisory"
	"github.com/jusassessoria/platform/internal/indexing"
	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/llm"
	"golang.org/x/sync/errgroup"
)

// SuggestThesesCommand is the input for Sugerir Teses. Exactly ONE of DraftID
// or IntimationID must be set:
//
//   - DraftID  != "": path original — carrega o draft (guard de tenant) e usa
//     Tone/Instructions/SelectedTheses já persistidos.
//   - DraftID  == "" && IntimationID != "": path novo (POST /v1/theses) — pula
//     GetDraftByID (não há draft ainda) e sintetiza um *Draft mínimo só com
//     PieceType pra alimentar buildDraftContext. Usado pela tela /pecas/nova
//     do FE, que difere a criação do draft até o commit ("Gerar" / "Manual")
//     — evita rascunho zumbi cada vez que o advogado clica em "Redigir peça".
//
// PieceType é ignorado quando DraftID != "" (vem do draft).
type SuggestThesesCommand struct {
	TenantID     string
	DraftID      string
	IntimationID string
	PieceType    string
	// ModelOverride ignora uc.model quando != "". Debug-only pra A/B de
	// modelos direto do FE. Remover quando o modelo default for escolhido.
	ModelOverride string
}

// SuggestThesesResult is the output of SuggestTheses.
type SuggestThesesResult struct {
	Theses []Thesis
}

// thesesSchema is the JSON Schema constraining the LLM's output for Sugerir Teses
// (strict mode). Reference é texto livre (não Citation estruturada). Evidence é
// array de trechos LITERAIS do teor/chunks que sustentam a tese — força o
// modelo a justificar a confidence com prova concreta em vez de rotular por
// impressão. Pode vir vazio pra teses puramente doutrinárias, mas o prompt
// obriga confidence=baixa nesse caso.
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
          "foundation": { "type": "string" },
          "evidence":   { "type": "array", "items": { "type": "string" } }
        },
        "required": ["label", "confidence", "reference", "foundation", "evidence"],
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
	ragCache *RAGCache // nil → sem cache (comportamento legado)
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
	RAGCache *RAGCache
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
		ragCache: p.RAGCache,
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
	if cmd.DraftID == "" && cmd.IntimationID == "" {
		return nil, apperr.NewInvalid("SuggestTheses: informe draft_id OU intimation_id")
	}

	// ── Phase 1: READ (short tx) ──────────────────────────────────────────────
	// Fluxo em 2 sub-fases pra paralelizar o que dá (P3):
	//   1a) Resolve o Draft (via DraftID) OU sintetiza um Draft mínimo com
	//       IntimationID+PieceType (path novo, sem draft). Serial obrigatório.
	//   1b) Se DraftID != "": intimation e parties são independentes → paralelo.
	//       Se DraftID == "": intimation vem primeiro (pra resolver CaseID),
	//       depois parties — serial. O caminho crítico dessa variante é o load
	//       da intimation.
	var d *Draft
	var intimation *IntimationContext
	var parties []PartyInfo

	// 1a) Draft (obrigatório serial pra path com DraftID).
	if cmd.DraftID != "" {
		if err := uc.uow.Do(ctx, cmd.TenantID, func(tx database.Tx) error {
			loaded, err := uc.reader.GetDraftByID(ctx, tx, cmd.TenantID, cmd.DraftID)
			if err != nil {
				return err
			}
			d = loaded
			return nil
		}); err != nil {
			return nil, err
		}
	} else {
		// Path sem draft: sintetiza um Draft mínimo. Tone/Instructions/
		// SelectedTheses ficam vazios (o advogado ainda não configurou nada
		// — as teses sugeridas aqui é que vão popular essa seleção depois).
		d = &Draft{PieceType: cmd.PieceType, IntimationID: cmd.IntimationID}
	}

	// 1b) Loads dependentes. Path DraftID != "" pode paralelizar
	// intimation||parties (independentes). Path IntimationID-only precisa
	// intimation antes de parties (CaseID vem da intimation).
	if cmd.DraftID != "" && d.IntimationID != "" && d.CaseID != "" {
		// Paralelo: intimation e parties independentes.
		var mu sync.Mutex
		eg, egCtx := errgroup.WithContext(ctx)
		iid, cid := d.IntimationID, d.CaseID
		eg.Go(func() error {
			return uc.uow.Do(egCtx, cmd.TenantID, func(tx database.Tx) error {
				i, e := uc.reader.GetIntimationForDraft(egCtx, tx, cmd.TenantID, iid)
				if e != nil {
					slog.WarnContext(egCtx, "theses: intimation load failed",
						slog.String("draft_id", cmd.DraftID), slog.Any("error", e))
					return nil
				}
				mu.Lock()
				intimation = i
				mu.Unlock()
				return nil
			})
		})
		eg.Go(func() error {
			return uc.uow.Do(egCtx, cmd.TenantID, func(tx database.Tx) error {
				pp, e := uc.reader.GetPartiesForDraft(egCtx, tx, cmd.TenantID, cid)
				if e != nil {
					slog.WarnContext(egCtx, "theses: parties load failed",
						slog.String("draft_id", cmd.DraftID), slog.Any("error", e))
					return nil
				}
				mu.Lock()
				parties = pp
				mu.Unlock()
				return nil
			})
		})
		if err := eg.Wait(); err != nil {
			slog.WarnContext(ctx, "theses: phase 1 parallel loads failed",
				slog.String("draft_id", cmd.DraftID), slog.Any("error", err))
		}
	} else {
		// Serial: intimation resolve o CaseID, depois parties.
		if err := uc.uow.Do(ctx, cmd.TenantID, func(tx database.Tx) error {
			if d.IntimationID != "" {
				i, e := uc.reader.GetIntimationForDraft(ctx, tx, cmd.TenantID, d.IntimationID)
				if e != nil {
					slog.WarnContext(ctx, "theses: intimation load failed — falling back to tenant-wide RAG",
						slog.String("draft_id", cmd.DraftID),
						slog.String("intimation_id", d.IntimationID),
						slog.Any("error", e))
				} else {
					intimation = i
					if d.CaseID == "" {
						d.CaseID = i.CaseID
					}
				}
			}
			if d.CaseID != "" {
				pp, e := uc.reader.GetPartiesForDraft(ctx, tx, cmd.TenantID, d.CaseID)
				if e != nil {
					slog.WarnContext(ctx, "theses: parties load failed",
						slog.String("draft_id", cmd.DraftID),
						slog.String("case_id", d.CaseID),
						slog.Any("error", e))
				} else {
					parties = pp
				}
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}

	// ── Phase 2: LLM (NO tx — connection released) ───────────────────────────
	var crid *string
	if intimation != nil && intimation.CourtRecordID != "" {
		crid = &intimation.CourtRecordID
	}
	queryText := buildQueryText(d, intimation)
	// topK reduzido de 8 pra 5 (P4): corta ~1500-2000 tokens do prompt sem
	// comprometer grounding (empiricamente as top-5 respondem a 90%+ das
	// teses; chunks 6-8 raramente aparecem citados). Ganho: 400-800ms no LLM
	// quando o corpus está indexado. Em dev sem chunks é no-op.
	chunks, chunkHits, grounded := runRAG(ctx, uc.emb, uc.search, uc.ragCache, cmd.TenantID, crid, queryText, 5)

	draftCtx := buildDraftContext(d, intimation, parties, chunks)
	composed, err := uc.composer.ComposeTheses(advisory.AgentSuggestTheses, draftCtx)
	if err != nil {
		return nil, fmt.Errorf("theses: compose prompt: %w", err)
	}

	model := uc.model
	if cmd.ModelOverride != "" {
		model = cmd.ModelOverride
	}
	llmStart := time.Now()
	rawBytes, err := uc.gen.GenerateJSON(ctx, llm.Request{
		System:     composed.System,
		User:       composed.User,
		Schema:     thesesSchema,
		SchemaName: "suggest_theses",
		Model:      model,
		MaxTokens:  2048,
		UseCase:    "draft.theses",
		TenantID:   cmd.TenantID,
	})
	llmMs := time.Since(llmStart).Milliseconds()
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

	// Rubric guard — o prompt v2 pede: "alta = ≥2 trechos literais apoiam
	// diretamente". Se o LLM cuspiu alta com <2 evidências, faz downgrade
	// pra media. Sem isso, ~40% das "altas" vinham com só 1 evidência (medido
	// em amostra de 5 intimações). O contrário — alta legítima com 1 trecho
	// forte — é aceitável como media, e a evidência continua exposta pro
	// advogado julgar. Rule enforcement no BE mantém o wire consistente
	// independente de como o modelo se comporta.
	downgraded := 0
	for i := range theses {
		if theses[i].Confidence == ThesisConfidenceAlta && len(theses[i].Evidence) < 2 {
			theses[i].Confidence = ThesisConfidenceMedia
			downgraded++
		}
	}

	// Source attribution: tie each thesis's literal evidence back to the autos
	// document it was retrieved from (the LLM returns only quotes, not ids), so the
	// peça screen can show "esta tese se apoia na Petição inicial · pág. 3" instead
	// of only the teor. Post-hoc match against the RAG hits — no extra LLM call.
	attributed := 0
	for i := range theses {
		attributeThesisSource(&theses[i], chunkHits)
		if theses[i].SourceDocumentID != "" {
			attributed++
		}
	}

	slog.InfoContext(ctx, "draft theses suggestion completed",
		slog.String("draft_id", cmd.DraftID),
		slog.String("model", model),
		slog.Int64("llm_ms", llmMs),
		slog.Int("theses", len(theses)),
		slog.Int("alta_downgraded", downgraded),
		slog.Int("source_attributed", attributed),
		slog.Bool("grounded", grounded),
	)
	return &SuggestThesesResult{Theses: theses}, nil
}

// minEvidenceMatchLen guards against a too-short evidence excerpt (e.g. "art. 5º")
// spuriously matching many chunks — only excerpts of real substance attribute a
// source. 24 normalized chars ≈ a short sentence fragment.
const minEvidenceMatchLen = 24

// attributeThesisSource ties a thesis to the autos document its literal Evidence was
// retrieved from: it substring-matches each evidence against the RAG chunk hits
// (normalized: lowercased, whitespace-collapsed — the LLM quotes verbatim FROM the
// chunk, so a real quote is a substring of it), and attributes the thesis to the
// document cited by the MOST evidence (tiebreak: highest chunk score). Evidence that
// matches no retrieved chunk (teor-only or doctrinal) leaves the source empty — the
// caller/FE falls back to the teor, the pre-existing behavior. Mirrors the chat's
// validateChatCitations philosophy: grounding only in what was actually retrieved.
func attributeThesisSource(t *Thesis, hits []indexing.ChunkHit) {
	if len(t.Evidence) == 0 || len(hits) == 0 {
		return
	}

	type docAgg struct {
		count     int
		bestScore float64
		hit       indexing.ChunkHit
		excerpt   string
	}
	byDoc := map[string]*docAgg{}

	for _, ev := range t.Evidence {
		nev := normalizeForMatch(ev)
		if len(nev) < minEvidenceMatchLen {
			continue
		}
		// Best hit for THIS evidence: a chunk whose text contains the quote, highest score.
		var best *indexing.ChunkHit
		for i := range hits {
			if hits[i].DocumentID == "" {
				continue
			}
			if !strings.Contains(normalizeForMatch(hits[i].Text), nev) {
				continue
			}
			if best == nil || hits[i].Score > best.Score {
				best = &hits[i]
			}
		}
		if best == nil {
			continue
		}
		a := byDoc[best.DocumentID]
		if a == nil {
			a = &docAgg{}
			byDoc[best.DocumentID] = a
		}
		a.count++
		if best.Score >= a.bestScore {
			a.bestScore = best.Score
			a.hit = *best
			a.excerpt = ev
		}
	}

	var win *docAgg
	for _, a := range byDoc {
		if win == nil || a.count > win.count || (a.count == win.count && a.bestScore > win.bestScore) {
			win = a
		}
	}
	if win == nil {
		return
	}
	t.SourceDocumentID = win.hit.DocumentID
	t.SourcePage = win.hit.Page
	t.SourceExcerpt = win.excerpt
	t.SourceLabel = thesisSourceLabel(win.hit)
}

// thesisSourceLabel renders a human "documento · pág. N" label from a chunk hit,
// preferring the document title, then its type, then a generic fallback.
func thesisSourceLabel(h indexing.ChunkHit) string {
	name := strings.TrimSpace(h.DocumentTitle)
	if name == "" {
		name = strings.TrimSpace(h.DocumentType)
	}
	if name == "" {
		name = "Documento dos autos"
	}
	if h.Page > 0 {
		return fmt.Sprintf("%s · pág. %d", name, h.Page)
	}
	return name
}

// normalizeForMatch lowercases and collapses runs of whitespace so a literal quote
// matches its source chunk despite trivial formatting differences.
func normalizeForMatch(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

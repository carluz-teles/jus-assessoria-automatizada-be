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
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"

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
// obriga confidence=baixa nesse caso. source_ref é o NÚMERO do trecho (1..N da
// lista "Trechos relevantes dos autos") de onde a evidence foi copiada — o
// modelo CITA a fonte em vez de re-casarmos por substring depois; 0 = tese
// funda-se só no teor ou em doutrina. maxItems=8 capa a lista.
var thesesSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "theses": {
      "type": "array",
      "maxItems": 8,
      "items": {
        "type": "object",
        "properties": {
          "label":      { "type": "string" },
          "confidence": { "type": "string", "enum": ["alta", "media", "baixa"] },
          "reference":  { "type": "string" },
          "foundation": { "type": "string" },
          "evidence":   { "type": "array", "items": { "type": "string" } },
          "source_ref": { "type": "integer" }
        },
        "required": ["label", "confidence", "reference", "foundation", "evidence", "source_ref"],
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

	// suggest_theses não usa a estrutura de seções do profile (só o mesmo case
	// signal do draft_minuta) — passa nil profile.
	draftCtx := buildDraftContext(d, intimation, parties, chunks, nil, nil)
	// teorText is the SAME cleaned teor the prompt shows (buildDraftContext
	// stripHTML's it into DraftContext.IntimationText) — reuse it verbatim so the
	// grounding validation matches exactly what the LLM read.
	teorText := draftCtx.IntimationText
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
		Model:       model,
		MaxTokens:   2048,
		Temperature: thesesTemperature,
		UseCase:     "draft.theses",
		TenantID:    cmd.TenantID,
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

	// Source attribution: the LLM CITES the chunk it quoted (source_ref), so we
	// resolve the source by an EXACT lookup into the RAG hits (hits[source_ref-1])
	// instead of re-matching by substring. resolveThesisSource also computes
	// Grounded: it verifies the literal evidence really is a substring of the
	// cited chunk (or of the teor when source_ref==0), killing false-grounded /
	// hallucinated evidence. No extra LLM call.
	attributed, groundedCount := 0, 0
	for i := range theses {
		resolveThesisSource(&theses[i], chunkHits, teorText)
		if theses[i].SourceDocumentID != "" {
			attributed++
		}
		if theses[i].Grounded {
			groundedCount++
		}
	}

	// Determinism: sort by confidence (alta>media>baixa) desc, then grounded-first,
	// stable on the original order; then cap at 8 (schema maxItems already asks the
	// model, but we enforce it defensively).
	sortTheses(theses)
	if len(theses) > maxTheses {
		theses = theses[:maxTheses]
	}

	slog.InfoContext(ctx, "draft theses suggestion completed",
		slog.String("draft_id", cmd.DraftID),
		slog.String("model", model),
		slog.Int64("llm_ms", llmMs),
		slog.Int("theses", len(theses)),
		slog.Int("alta_downgraded", downgraded),
		slog.Int("source_attributed", attributed),
		slog.Int("grounded_theses", groundedCount),
		slog.Bool("grounded", grounded),
	)
	return &SuggestThesesResult{Theses: theses}, nil
}

// thesesTemperature is the low sampling temperature for the thesis-suggestion
// call: teses são um julgamento jurídico que queremos estável e reprodutível,
// não criativo — variância alta só produz teses que oscilam entre execuções.
const thesesTemperature = 0.2

// maxTheses caps the suggested-thesis list (mirrors thesesSchema maxItems).
const maxTheses = 8

// resolveThesisSource resolves a thesis's source by the LLM's OWN citation
// (SourceRef, the 1-based chunk number) instead of re-matching evidence by
// substring — grounding is trustworthy "by construction". It also validates the
// evidence against what the model claimed to read and sets Grounded:
//
//   - SourceRef in [1, len(hits)] → the thesis cites a retrieved chunk.
//     hit = hits[SourceRef-1]; Source{DocumentID,Page,Label} come from it.
//     SourceExcerpt = the first Evidence that is a (robust) substring of hit.Text;
//     if none matches we keep the 1st evidence but mark Grounded=false (the ref was
//     given yet no evidence casa no trecho citado → likely wrong/hallucinated chunk).
//     Grounded=true ONLY when some evidence really is a substring of hit.Text.
//   - SourceRef==0 or out of range → no document. If some evidence is a substring
//     of teorText (the intimation teor), Grounded=true (ancorada no teor, no doc —
//     the FE falls back to the teor). If it matches nowhere → Grounded=false AND we
//     CLEAR SourceExcerpt (never show the advogado an invented "trecho literal").
func resolveThesisSource(t *Thesis, hits []indexing.ChunkHit, teorText string) {
	if t.SourceRef >= 1 && t.SourceRef <= len(hits) {
		hit := hits[t.SourceRef-1]
		t.SourceDocumentID = hit.DocumentID
		t.SourcePage = hit.Page
		t.SourceLabel = thesisSourceLabel(hit)

		nhit := normalizeForMatch(hit.Text)
		matched := ""
		for _, ev := range t.Evidence {
			if nev := normalizeForMatch(ev); nev != "" && strings.Contains(nhit, nev) {
				matched = ev
				break
			}
		}
		if matched != "" {
			t.SourceExcerpt = matched
			t.Grounded = true
			return
		}
		// Ref given but no evidence casa no trecho citado: trust the ref (keep the
		// source) but flag it as not verified. Show the 1st evidence as excerpt.
		if len(t.Evidence) > 0 {
			t.SourceExcerpt = t.Evidence[0]
		}
		t.Grounded = false
		return
	}

	// No document cited (0 or out of range): try to ground in the teor.
	nteor := normalizeForMatch(teorText)
	for _, ev := range t.Evidence {
		if nev := normalizeForMatch(ev); nteor != "" && nev != "" && strings.Contains(nteor, nev) {
			t.Grounded = true // ancorada no teor; no SourceDocumentID (FE cai no teor)
			return
		}
	}
	// Matched nothing anywhere → evidence provavelmente alucinada.
	t.Grounded = false
	t.SourceExcerpt = ""
}

// confidenceRank orders the closed confidence set for the deterministic sort
// (alta first). Unknown/empty ranks last.
func confidenceRank(c string) int {
	switch c {
	case ThesisConfidenceAlta:
		return 0
	case ThesisConfidenceMedia:
		return 1
	case ThesisConfidenceBaixa:
		return 2
	default:
		return 3
	}
}

// sortTheses orders the theses deterministically: confidence (alta>media>baixa)
// descending, then Grounded=true before false, keeping the original order stable
// among equals (so the same input always yields the same wire order).
func sortTheses(theses []Thesis) {
	sort.SliceStable(theses, func(i, j int) bool {
		ri, rj := confidenceRank(theses[i].Confidence), confidenceRank(theses[j].Confidence)
		if ri != rj {
			return ri < rj
		}
		if theses[i].Grounded != theses[j].Grounded {
			return theses[i].Grounded // true sorts before false
		}
		return false // stable on original order
	})
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

// normalizeForMatch makes the substring-validation of evidence robust to trivial
// formatting differences between the LLM's quote and its source text: it strips
// diacritics (NFD + drop combining marks), lowercases, drops punctuation/quotes/
// hyphens (curly quotes “ ” ‘ ’ included, via unicode.IsPunct), and collapses runs
// of whitespace. So "São Paulo — 'réu'" and "sao paulo reu" compare equal.
func normalizeForMatch(s string) string {
	// Strip diacritics: decompose, drop combining marks, recompose.
	t := transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	if out, _, err := transform.String(t, s); err == nil {
		s = out
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		switch {
		case unicode.IsSpace(r):
			b.WriteRune(' ')
		case unicode.IsPunct(r) || unicode.IsSymbol(r):
			// drop punctuation/quotes/hyphens/symbols
		default:
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

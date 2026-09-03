package draft

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jusassessoria/platform/internal/advisory"
	"github.com/jusassessoria/platform/internal/indexing"
	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
	"github.com/jusassessoria/platform/lib/llm"
	"golang.org/x/sync/errgroup"
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
// (GetDraftByID + optionally GetIntimationForDraft + GetPartiesForDraft), but kept as
// a port so the unit tests can inject a fake without a full repo mock.
type generationDepsReader interface {
	GetDraftByID(ctx context.Context, tx database.Tx, tenantID, draftID string) (*Draft, error)
	GetIntimationForDraft(ctx context.Context, tx database.Tx, tenantID, intimationID string) (*IntimationContext, error)
	GetPartiesForDraft(ctx context.Context, tx database.Tx, tenantID, caseID string) ([]PartyInfo, error)
	// ListSuggestedThesesByDraft feeds the generation selection from the PERSISTED
	// thesis state (C2): the composer's SelectedTheses is derived from the theses in
	// state included/pending_add, not from the fragile draft.selected_theses column.
	ListSuggestedThesesByDraft(ctx context.Context, tx database.Tx, tenantID, draftID string) ([]SuggestedThesis, error)
	// ListSuggestedThesisAnchorsByDraft loads the N anchors of a draft's theses in one
	// query (multi-âncora, 0094) so the composer lists every autos document that backs
	// each selected thesis, not only the primary.
	ListSuggestedThesisAnchorsByDraft(ctx context.Context, tx database.Tx, tenantID, draftID string) (map[string][]ThesisAnchor, error)
	// GetGenerationProfile loads the piece_profile + ordered sections for the draft's
	// piece_profile_key (PART B) so the composer renders the catalog structure. nil
	// (unknown/empty key) → the composer falls back to the generic structure.
	GetGenerationProfile(ctx context.Context, tx database.Tx, pieceProfileKey string) (*GenerationProfile, error)
}

// generationWriter is the narrow write port the generation use case needs. A separate
// interface (not the full Repository) keeps the fake in tests minimal.
type generationWriter interface {
	UpdateSagaState(ctx context.Context, tx database.Tx, draftID, tenantID, sagaState string, updateContent bool, content string, structured *StructuredContent) (*Draft, error)
	UpdateDraftContentHtml(ctx context.Context, tx database.Tx, draftID, tenantID, html string) error
	// SetDraftContentEdited reseta o flag de edição manual (0096) na geração:
	// a peça recém-gerada não tem ajuste manual.
	SetDraftContentEdited(ctx context.Context, tx database.Tx, draftID, tenantID string, edited bool) error
	InsertReview(ctx context.Context, tx database.Tx, r *Review) (*Review, error)
	DeleteReviewsForDraft(ctx context.Context, tx database.Tx, draftID string) error
	// Segment persistence (thesis↔trecho, 0095): DeleteSuggestedThesisSegmentsByDraft
	// clears prior segments before a regenerate; InsertSuggestedThesisSegment persists
	// one matched section as a thesis's segment. Both run in the generation success tx.
	DeleteSuggestedThesisSegmentsByDraft(ctx context.Context, tx database.Tx, tenantID, draftID string) error
	InsertSuggestedThesisSegment(ctx context.Context, tx database.Tx, tenantID, draftID, thesisID string, s *ThesisSegment, position int) (*ThesisSegment, error)
}

// embedder is the narrow embedding port for RAG. Satisfied by *indexing.VoyageEmbedder
// or a test fake. nil → degraded path (no grounding).
type embedder interface {
	Embed(ctx context.Context, texts []string, inputType indexing.InputType) ([][]float32, string, error)
}

// VoyageEmbedder is the exported type alias so cmd/worker-ai can use *indexing.VoyageEmbedder
// as the embedder port without an import cycle. The concrete type satisfies this interface.
type VoyageEmbedder = embedder

// GenerateUseCase is the async handler for draft.generation_requested events. It holds
// all its ports as interfaces so tests inject fakes — the real LLM/embedder APIs are
// NEVER called under test.
// chunkPublisher é a porta narrow que o worker usa pra empurrar chunks do
// LLM num Redis Stream (XADD com MAXLEN + EXPIRE). Cada chunk vira um entry
// com ID sequencial — o SSE handler pode retomar via Last-Event-ID quando o
// cliente reconecta, sem perder chunks. Satisfeita por lib/pubsub.StreamPublisher.
// Injetada como Option (nil → geração roda em modo batch, sem streaming).
type chunkPublisher interface {
	XPublish(ctx context.Context, streamKey string, payload []byte) (string, error)
	XReset(ctx context.Context, streamKey string) error
}

// chunkChannel monta o nome do stream Redis pra um draft — usado pelo
// worker (produtor) e pelo endpoint SSE (consumidor) sem depender um do outro.
func chunkChannel(draftID string) string { return "draft:" + draftID + ":stream" }

// StreamResetMarker é o 1º chunk publicado a cada (re)geração: sinaliza ao FE que
// uma nova geração começou e o buffer acumulado (possível replay stale da geração
// anterior) deve ser zerado. U+241E (␞, record separator) não aparece em markdown
// jurídico e é transportado 1:1 pelo SSE (sem \n → um único `data:`).
const StreamResetMarker = "␞"

type GenerateUseCase struct {
	uow      database.UnitOfWork
	reader   generationDepsReader
	writer   generationWriter
	outbox   outboxPublisher
	dedup    generateDeduper
	gen      llm.Generator       // nil → FAILED "IA não configurada"
	emb      embedder            // nil → degraded (no grounding)
	search   indexing.SearchDeps // Pool may be nil → degraded
	ragCache *RAGCache           // nil → sem cache
	composer advisory.PromptComposer
	chunkPub chunkPublisher // nil → geração em modo batch (não streama)
	model    string         // OpenRouter model slug (from config); falls back to generationModel
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
	RAGCache *RAGCache
	Composer advisory.PromptComposer
	ChunkPub chunkPublisher   // nil → sem streaming
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
		ragCache: p.RAGCache,
		composer: p.Composer,
		chunkPub: p.ChunkPub,
		model:    p.Model,
		now:      p.Now,
	}
}

// generatedOutput is the LLM's structured response for one generation call.
// v8: DraftMarkdown carrega markdown (CommonMark + GFM tables). O worker
// converte pra HTML via goldmark antes de persistir em content_html —
// markdown streama char-a-char sem corromper (padrão da indústria pra LLM).
type generatedOutput struct {
	DraftMarkdown string `json:"draft_markdown"`
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
// structured output (strict). v8: draft_markdown é o markdown da peça
// (CommonMark + GFM tables). O worker converte pra HTML via goldmark antes
// de persistir. Streaming char-a-char funciona porque markdown não tem
// tags pareadas que se corrompam ao serem cortadas no meio.
var generateSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "draft_markdown": { "type": "string" }
  },
  "required": ["draft_markdown"],
  "additionalProperties": false
}`)

// OnGenerationRequested is the async entry point called by the worker-ai listener.
// It:
//  1. Dedups (processed_event guard — consumer "draft_ai").
//  2. Reloads the draft; guards saga_state == EXTRACTING (skip if obsolete).
//  3. Embeds the intimation text and runs RAG (degraded if embedder nil or empty result).
//  4. Calls the LLM to generate draft_content.
//  5. Persists: UPDATE draft SET content_html=..., saga_state=DRAFTED; mark
//     processed_event; outbox.Publish(draft.generated) — all in ONE tx.
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
	// Phase 1 rodada em 2 etapas pra paralelizar o que dá (P3):
	//   3a) GetDraftByID + guard saga (obrigatório serial — precisa saber
	//       IntimationID/CaseID antes das outras loads).
	//   3b) GetIntimationForDraft e GetPartiesForDraft rodam em PARALELO
	//       via errgroup, cada uma em tx própria (short reads, safe).
	//       Reads são independentes entre si; ganho: ~15-25ms em prod.
	var draft *Draft
	var genProfile *GenerationProfile
	var richTheses []SuggestedThesis
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
		// C2 — a seleção que alimenta o composer vem do ESTADO PERSISTIDO das teses
		// (suggested_thesis em included/pending_add), não do frágil draft.selected_theses.
		// Quando há teses persistidas selecionadas elas VENCEM; sem nenhuma persistida,
		// mantém d.SelectedTheses (retrocompat com o caminho legado Fatia 5).
		theses, e := uc.reader.ListSuggestedThesesByDraft(ctx, tx, ev.TenantID, ev.DraftID)
		if e != nil {
			return e
		}
		// Attach the N anchors per thesis (multi-âncora, 0094) so the composer lists
		// every autos document backing each selected tese.
		if anchorsByThesis, ae := uc.reader.ListSuggestedThesisAnchorsByDraft(ctx, tx, ev.TenantID, ev.DraftID); ae != nil {
			return ae
		} else {
			for i := range theses {
				theses[i].Anchors = anchorsByThesis[theses[i].ID]
			}
		}
		if labels := selectedThesisLabels(theses); labels != nil {
			draft.SelectedTheses = labels
		}
		// Rich selected theses (com Foundation/Reference/Source*) pra o composer
		// desenvolver cada tese do material ancorado, não só do rótulo. Mesmo filtro/
		// estado que os labels acima; nil quando não há persistidas selecionadas → o
		// buildDraftContext cai no fallback legado (só-labels de draft.SelectedTheses).
		richTheses = selectedThesesForGen(theses)
		// PART B — carrega o piece_profile + seções ordenadas do catálogo pra o
		// composer renderizar o MIOLO real (Preliminares/Impugnação/Mérito/…) em vez
		// do trio genérico. Só quando o draft tem piece_profile_key; vazio/desconhecido
		// → gp fica nil e o composer cai no genérico (retrocompat com drafts legados).
		// Non-fatal: profile é enriquecimento, não bloqueia a geração.
		if d.PieceProfileKey != "" {
			gp, e := uc.reader.GetGenerationProfile(ctx, tx, d.PieceProfileKey)
			if e != nil {
				slog.WarnContext(ctx, "draft generate: profile load failed",
					slog.String("draft_id", ev.DraftID),
					slog.String("piece_profile_key", d.PieceProfileKey),
					slog.Any("error", e))
			} else {
				genProfile = gp
			}
		}
		return nil
	}); err != nil {
		if isErrObsoleteSaga(err) {
			return fmt.Errorf("%w: %w", err, errSkipRetry)
		}
		return fmt.Errorf("draft generate: load draft: %w", err)
	}

	// 3b) Loads paralelos — cada goroutine tem tx própria (uow.Do pega conn
	// do pool). Ambos non-fatal: warn + segue ungrounded/sem-partes.
	var intimation *IntimationContext
	var parties []PartyInfo
	var mu sync.Mutex // protege intimation/parties (paranoia; errgroup já ordena)
	eg, egCtx := errgroup.WithContext(ctx)
	if draft.IntimationID != "" {
		iid := draft.IntimationID
		eg.Go(func() error {
			return uc.uow.Do(egCtx, ev.TenantID, func(tx database.Tx) error {
				i, e := uc.reader.GetIntimationForDraft(egCtx, tx, ev.TenantID, iid)
				if e != nil {
					slog.WarnContext(egCtx, "draft generate: intimation load failed",
						slog.String("draft_id", ev.DraftID), slog.Any("error", e))
					return nil // non-fatal
				}
				mu.Lock()
				intimation = i
				mu.Unlock()
				return nil
			})
		})
	}
	if draft.CaseID != "" {
		cid := draft.CaseID
		eg.Go(func() error {
			return uc.uow.Do(egCtx, ev.TenantID, func(tx database.Tx) error {
				pp, e := uc.reader.GetPartiesForDraft(egCtx, tx, ev.TenantID, cid)
				if e != nil {
					slog.WarnContext(egCtx, "draft generate: parties load failed",
						slog.String("draft_id", ev.DraftID), slog.Any("error", e))
					return nil // non-fatal
				}
				mu.Lock()
				parties = pp
				mu.Unlock()
				return nil
			})
		})
	}
	// Erros aqui só acontecem se uow.Do falhar (conexão morta) — as queries
	// individuais degradam via warn + nil. Ignore-safe.
	if err := eg.Wait(); err != nil {
		slog.WarnContext(ctx, "draft generate: phase 1 parallel loads failed",
			slog.String("draft_id", ev.DraftID), slog.Any("error", err))
	}

	// ── 4. RAG: embed + search chunks ─────────────────────────────────────────
	// Resolve the court_record_id from the already-loaded intimation context so
	// SearchChunks scopes to this process's documents (intimation-scoped grounding).
	// When there is no intimation (blank/processo draft), crid stays nil and the
	// search spans the whole tenant corpus (the existing behaviour, preserved).
	var crid *string
	if intimation != nil && intimation.CourtRecordID != "" {
		crid = &intimation.CourtRecordID
	}
	queryText := buildQueryText(draft, intimation)
	chunks, _, grounded := runRAG(ctx, uc.emb, uc.search, uc.ragCache, ev.TenantID, crid, queryText, 8)

	// ── 5. Compose prompt and call LLM ────────────────────────────────────────
	draftCtx := buildDraftContext(draft, intimation, parties, chunks, genProfile, richTheses)
	composed, err2 := uc.composer.ComposeDraft(advisory.AgentDraftMinuta, draftCtx)
	if err2 != nil {
		return uc.persistFailure(ctx, ev.TenantID, ev.DraftID, ev.EventID, fmt.Sprintf("compose prompt: %v", err2))
	}

	// STREAMING — publica cada chunk bruto no canal pub/sub `draft:<id>:stream`
	// pra o SSE do api encaminhar ao FE. Ao final, `rawBytes` carrega a
	// resposta completa (idêntica ao modo batch) pra fazer json.Unmarshal.
	// Se o Publisher não foi wireado (uc.chunkPub == nil), degrada pra batch
	// mode (mesmo comportamento anterior).
	// callLLM roda UMA geração (streaming quando o Publisher está wireado, senão
	// batch). Extraído num closure porque a geração é RE-EXECUTÁVEL: o Gemini, sob
	// structured output, às vezes encerra a string do JSON cedo (finish_reason=stop,
	// peça truncada mid-frase). O reset do stream roda a cada tentativa — sem isso um
	// retry APENDARIA à saída truncada anterior no canal SSE.
	// SEM Schema → TEXTO LIVRE (markdown puro). Structured output (json_schema) com
	// uma string gigante truncava de forma intermitente (o modelo encerrava a string
	// do JSON cedo); markdown puro não tem string JSON pra cortar. O prompt já pede
	// markdown sem code fence; o parser de seções abaixo é o mesmo.
	req := llm.Request{
		System:    composed.System,
		User:      composed.User,
		Model:     uc.model,
		MaxTokens: 16000,
		UseCase:   "draft.generate",
		TenantID:  ev.TenantID,
	}
	callLLM := func() ([]byte, error) {
		if uc.chunkPub == nil {
			return uc.gen.GenerateJSON(ctx, req)
		}
		channel := chunkChannel(ev.DraftID)
		// Reset do stream antes de publicar — sem isso um cliente SSE recém-conectado
		// receberia replay dos chunks da geração anterior (TTL 10min). Ignora erro.
		if resetErr := uc.chunkPub.XReset(ctx, channel); resetErr != nil {
			slog.WarnContext(ctx, "draft chunk stream reset failed",
				slog.String("draft_id", ev.DraftID),
				slog.String("err", resetErr.Error()))
		}
		// Marcador de RESET como 1º chunk da geração. O XReset acima não basta: numa
		// REGERAÇÃO o cliente SSE tipicamente conecta ANTES do worker resetar (o saga
		// vira EXTRACTING de forma síncrona, mas o worker leva ~1-2s no RAG), então
		// ele lê os chunks da peça ANTERIOR do stream (replay de 0-0). Ao receber este
		// marcador (que só aparece no início da geração ATUAL), o FE zera o buffer —
		// descarta o replay stale e passa a acumular só o texto novo.
		if _, e := uc.chunkPub.XPublish(ctx, channel, []byte(StreamResetMarker)); e != nil {
			slog.WarnContext(ctx, "draft chunk reset marker publish failed",
				slog.String("draft_id", ev.DraftID), slog.String("err", e.Error()))
		}
		return uc.gen.GenerateJSONStream(ctx, req, func(chunk string) error {
			// Best-effort: erro no XADD não aborta. Cliente que reconecta usa
			// Last-Event-ID; se o key expirou, faz refetch quando saga=DRAFTED.
			if _, pubErr := uc.chunkPub.XPublish(ctx, channel, []byte(chunk)); pubErr != nil {
				slog.WarnContext(ctx, "draft chunk publish failed",
					slog.String("draft_id", ev.DraftID),
					slog.String("err", pubErr.Error()))
			}
			return nil
		})
	}

	// Guarda de robustez: até maxGenAttempts, refaz a geração quando a saída volta
	// TRUNCADA (JSON não-parseável OU sem o fecho "Pede deferimento" que toda peça
	// completa tem). Cobre o early-stop intermitente do modelo sem falhar a peça.
	const maxGenAttempts = 2
	var rawBytes []byte
	var err3 error
	var out generatedOutput
	for attempt := 1; attempt <= maxGenAttempts; attempt++ {
		rawBytes, err3 = callLLM()
		if err3 != nil {
			break // erro real de LLM — tratado abaixo (terminal vs retryable)
		}
		// Texto livre: o corpo JÁ é o markdown. Limpa code fence acidental.
		out = generatedOutput{DraftMarkdown: stripCodeFence(string(rawBytes))}
		if genOutputComplete(out.DraftMarkdown) {
			break // peça completa (chegou no fecho)
		}
		if attempt < maxGenAttempts {
			slog.WarnContext(ctx, "draft generate: saída truncada (fecho ausente), refazendo",
				slog.String("draft_id", ev.DraftID),
				slog.Int("attempt", attempt),
				slog.Int("markdown_len", len(out.DraftMarkdown)))
			continue
		}
		// Esgotou as tentativas ainda truncado — persiste o parcial (melhor que
		// FAILED; o advogado revisa/refaz) mas registra pra observabilidade.
		slog.WarnContext(ctx, "draft generate: ainda truncada após retries, persistindo parcial",
			slog.String("draft_id", ev.DraftID),
			slog.Int("markdown_len", len(out.DraftMarkdown)))
	}
	if err3 != nil {
		// Transient LLM errors stay retryable; terminal (bad key / parse) become FAILED.
		if isTerminalGenErr(err3) {
			return uc.persistFailure(ctx, ev.TenantID, ev.DraftID, ev.EventID, fmt.Sprintf("llm: %v", err3))
		}
		return fmt.Errorf("draft generate: llm call: %w", err3)
	}

	// ── 6. Persist success in ONE tx ──────────────────────────────────────────
	// Gerar now produces ONLY draft_content (DRAFTED state). Any previous reviews
	// (from prior generation attempts) are deleted so Revisar always operates on a
	// clean slate. The advogado triggers Revisar explicitly as a separate action.
	if err5 := uc.uow.Do(ctx, ev.TenantID, func(tx database.Tx) error {
		// Dedup in the SAME tx as the effect: a concurrent delivery that already
		// marked this event skips the writes.
		if seen, e := uc.dedup.SeenOrMark(ctx, tx, generationConsumer, ev.EventID); e != nil {
			return e
		} else if seen {
			return nil
		}
		// Delete any reviews from prior generation attempts so the draft starts
		// clean for the next Revisar call.
		if e := uc.writer.DeleteReviewsForDraft(ctx, tx, ev.DraftID); e != nil {
			return fmt.Errorf("delete prior reviews: %w", e)
		}
		// v8 (streaming markdown): o LLM gera markdown; convertemos aqui pra HTML
		// via goldmark antes de persistir. Streaming char-a-char do markdown
		// funciona porque não há tags pareadas — o FE consome os deltas com
		// tiptap-markdown/streamContent sem corromper.
		htmlOut, mdErr := markdownToHTML(out.DraftMarkdown)
		if mdErr != nil {
			return fmt.Errorf("convert markdown to html: %w", mdErr)
		}
		plainText := stripHTMLTagsForSearch(htmlOut)
		structured := parseHTMLToStructured(htmlOut)
		if e := uc.writer.UpdateDraftContentHtml(ctx, tx, ev.DraftID, ev.TenantID, htmlOut); e != nil {
			return fmt.Errorf("update content_html: %w", e)
		}
		// Peça recém-gerada não tem edição manual — reseta o flag (0096).
		if e := uc.writer.SetDraftContentEdited(ctx, tx, ev.DraftID, ev.TenantID, false); e != nil {
			return fmt.Errorf("reset content_edited: %w", e)
		}
		if _, e := uc.writer.UpdateSagaState(ctx, tx, ev.DraftID, ev.TenantID, SagaStateDrafted, true, plainText, structured); e != nil {
			return fmt.Errorf("update saga state: %w", e)
		}

		// Map each selected thesis to the SECTION of the generated peça it produced
		// (thesis↔segment, 0095) so the removal moldura shows the real text, not just
		// the truncated foundation. Deterministic match (heading↔label) over the
		// already-parsed structured content — no extra LLM call. Delete-then-insert so
		// a regenerate replaces stale segments. Best-effort: a thesis whose heading
		// diverges simply gets no segment (FE falls back to the foundation).
		if e := uc.writer.DeleteSuggestedThesisSegmentsByDraft(ctx, tx, ev.TenantID, ev.DraftID); e != nil {
			return fmt.Errorf("delete prior segments: %w", e)
		}
		for _, m := range matchThesisSegments(richTheses, structured) {
			if _, e := uc.writer.InsertSuggestedThesisSegment(ctx, tx, ev.TenantID, ev.DraftID, m.ThesisID, &m.Segment, m.Segment.Position); e != nil {
				return fmt.Errorf("insert thesis segment: %w", e)
			}
		}

		// Announce the successful generation in the SAME tx (transactional outbox).
		// Optional: a nil outbox (not wired) simply skips publishing — Gerar itself
		// never fails for it. Consumers: acquisition's activity listener (process
		// cockpit "Atividade" timeline — "Peça gerada"). court_record_id is carried
		// when the intimation was already resolved above (the common, intimation-
		// sourced draft case); a blank/processo draft leaves it empty and the
		// consumer falls back to its own resolution.
		if uc.outbox != nil {
			var crid string
			if intimation != nil {
				crid = intimation.CourtRecordID
			}
			pubEv := newDraftGenerated(ev.DraftID, ev.TenantID, crid)
			if e := uc.outbox.Publish(ctx, tx, pubEv); e != nil {
				return fmt.Errorf("publish draft.generated: %w", e)
			}
		}
		return nil
	}); err5 != nil {
		return fmt.Errorf("draft generate: persist success: %w", err5)
	}

	slog.InfoContext(ctx, "draft generation completed",
		slog.String("draft_id", ev.DraftID),
		slog.String("saga_state", SagaStateDrafted),
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
		if _, e := uc.writer.UpdateSagaState(ctx, tx, draftID, tenantID, SagaStateFailed, false, "", nil); e != nil {
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

// queryTeorMaxRunes bounds how much of the intimation teor feeds the RAG query. ~600 runes covers
// the operative opening of a typical DJEN teor (the parties, the act, the order) — enough signal
// to steer retrieval to the right chunks — without diluting the embedding with the boilerplate
// tail. Runes (not bytes) so a cut never splits a UTF-8 multi-byte codepoint (acentos). The bound
// also keeps the query DETERMINISTIC: same intimation → same query → stable RAG cache key
// (rag_cache.go hashes queryText), so we never inject anything volatile.
const queryTeorMaxRunes = 600

// buildQueryText builds the RAG query string from the draft and intimation context. Beyond the
// PieceType + Type it enriches the query with the CASE signal — the classe/assunto and a bounded,
// HTML-stripped slice of the intimation teor — so the query embeds close to the autos chunks that
// actually matter (a bare "PETICAO INTIMACAO" query recalls almost anything). Everything here is
// deterministic and length-bounded: no timestamps, ids, or other volatile input that would poison
// the RAG cache key.
func buildQueryText(d *Draft, i *IntimationContext) string {
	parts := make([]string, 0, 5)
	parts = append(parts, d.PieceType)
	if i != nil {
		if i.Type != "" {
			parts = append(parts, i.Type)
		}
		if i.Class != "" {
			parts = append(parts, i.Class)
		}
		if i.Subject != "" {
			parts = append(parts, i.Subject)
		}
		// The teor is the richest case signal. Clean it through the same stripHTML the prompt
		// uses (single source of truth), then cap to queryTeorMaxRunes on a rune boundary.
		if teor := boundedRunes(stripHTML(i.Content), queryTeorMaxRunes); teor != "" {
			parts = append(parts, teor)
		}
	}
	return strings.Join(parts, " ")
}

// boundedRunes returns s truncated to at most max runes, cutting on a rune boundary (never mid
// codepoint). A non-positive max or an already-short s returns s unchanged.
func boundedRunes(s string, max int) string {
	if max <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

// selectedThesisLabels derives the generation selection from the PERSISTED thesis
// state (C2): the labels of the theses in state included/pending_add, in position
// order. Returns nil when the draft has NO persisted theses at all — the caller then
// keeps the legacy draft.selected_theses (Fatia 5 backward-compat). When the draft HAS
// persisted theses but none is selected, it returns a non-nil empty slice so the
// caller overrides with an empty selection (the user deselected everything).
func selectedThesisLabels(theses []SuggestedThesis) []string {
	if len(theses) == 0 {
		return nil
	}
	labels := make([]string, 0, len(theses))
	for _, t := range theses {
		if t.State == ThesisStateIncluded || t.State == ThesisStatePendingAdd {
			labels = append(labels, t.Label)
		}
	}
	return labels
}

// selectedThesesForGen returns the SELECTED theses (state included/pending_add) in
// full — same filter as selectedThesisLabels, but keeping the rich Foundation/
// Reference/Source* fields the composer needs to develop each tese from anchored
// material instead of only its label. Ordered by Position (deterministic). Returns
// nil when there are no persisted theses OR none is selected — the caller then falls
// back to the legacy label-only path (draft.SelectedTheses).
func selectedThesesForGen(theses []SuggestedThesis) []SuggestedThesis {
	if len(theses) == 0 {
		return nil
	}
	sel := make([]SuggestedThesis, 0, len(theses))
	for _, t := range theses {
		if t.State == ThesisStateIncluded || t.State == ThesisStatePendingAdd {
			sel = append(sel, t)
		}
	}
	if len(sel) == 0 {
		return nil
	}
	sort.SliceStable(sel, func(i, j int) bool { return sel[i].Position < sel[j].Position })
	return sel
}

// buildDraftContext converts the loaded domain objects to the advisory DraftContext.
// When i is nil (blank/processo draft) the intimation fields remain empty strings —
// the prompt's add() helper drops empty labels, so the LLM still gets a clean prompt.
// parties may be nil/empty (non-fatal degraded path); the signing lawyer is resolved
// from the matched recipient in intimation.recipients (nil recipients → zero-value).
//
// genOutputComplete reports whether the generated markdown reached the FECHO. Toda
// peça completa termina no "Pede deferimento." (fecho do base_skeleton, v6); quando o
// modelo encerra cedo (early-stop intermitente sob structured output, finish_reason=
// stop mid-frase), o fecho não aparece — sinal determinístico de truncamento. Case-
// insensitive e por "deferimento" pra cobrir variações ("requer o deferimento",
// "termos em que ... pede deferimento").
func genOutputComplete(markdown string) bool {
	return strings.Contains(strings.ToLower(markdown), "deferimento")
}

// stripCodeFence remove um code fence markdown acidental (```markdown ... ``` ou
// ``` ... ```) que o modelo às vezes envolve, apesar da instrução de markdown puro.
// Sem fence → devolve o texto apenas trimado.
func stripCodeFence(s string) string {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "```") {
		return t
	}
	if nl := strings.IndexByte(t, '\n'); nl >= 0 {
		t = t[nl+1:]
	}
	t = strings.TrimSuffix(strings.TrimSpace(t), "```")
	return strings.TrimSpace(t)
}

// selectedTheses are the RICH selected theses (state included/pending_add, Position-
// ordered) mapped to advisory.SelectedThesisCtx. When it is empty BUT d.SelectedTheses
// (legacy label-only, Fatia 5) is non-empty, this builds ctx com só o Label preenchido —
// preservando o caminho legado sem quebrar. Empty em ambos → dc.SelectedTheses nil.
func buildDraftContext(d *Draft, i *IntimationContext, parties []PartyInfo, chunks []string, profile *GenerationProfile, selectedTheses []SuggestedThesis) advisory.DraftContext {
	dc := advisory.DraftContext{
		PieceType:       d.PieceType,
		PieceProfileKey: d.PieceProfileKey,
		Chunks:          chunks,
		// Fatia 5: tone/instructions are read from the draft row (not the event
		// payload) — TriggerGeneration persisted them in the same tx it flipped
		// saga_state to EXTRACTING.
		Tone:         d.Tone,
		Instructions: d.Instructions,
	}
	// Selected theses — rich path (Foundation/Reference/Source*) quando há teses
	// persistidas selecionadas; fallback legado só-com-Label a partir dos labels
	// persistidos em draft.SelectedTheses (caminho Fatia 5). Vazio total → nil.
	switch {
	case len(selectedTheses) > 0:
		ctx := make([]advisory.SelectedThesisCtx, 0, len(selectedTheses))
		for _, t := range selectedTheses {
			var anchors []advisory.ThesisAnchorCtx
			for _, a := range t.Anchors {
				anchors = append(anchors, advisory.ThesisAnchorCtx{
					Label:   a.Label,
					Excerpt: a.Excerpt,
				})
			}
			ctx = append(ctx, advisory.SelectedThesisCtx{
				Label:       t.Label,
				Foundation:  t.Foundation,
				Reference:   t.Reference,
				Excerpt:     t.SourceExcerpt,
				SourceLabel: t.SourceLabel,
				Grounded:    t.Grounded,
				Anchors:     anchors,
			})
		}
		dc.SelectedTheses = ctx
	case len(d.SelectedTheses) > 0:
		ctx := make([]advisory.SelectedThesisCtx, 0, len(d.SelectedTheses))
		for _, label := range d.SelectedTheses {
			ctx = append(ctx, advisory.SelectedThesisCtx{Label: label})
		}
		dc.SelectedTheses = ctx
	}
	// Populate structured parties (PLAINTIFF/DEFENDANT/THIRD_PARTY).
	if len(parties) > 0 {
		partiesCtx := make([]advisory.PartyCtx, 0, len(parties))
		for _, p := range parties {
			partiesCtx = append(partiesCtx, advisory.PartyCtx{
				Role:    p.Role,
				Name:    p.Name,
				Counsel: p.Counsel,
			})
		}
		dc.Parties = partiesCtx
	}
	// PART B — profile sections (catalog MIOLO). nil profile → dc.ProfileSections
	// stays nil and the composer renders the generic Fatos/Direito/Pedidos trio.
	if profile != nil && len(profile.Sections) > 0 {
		secs := make([]advisory.ProfileSectionCtx, 0, len(profile.Sections))
		for _, s := range profile.Sections {
			secs = append(secs, advisory.ProfileSectionCtx{
				Key:         s.Key,
				Titulo:      s.Titulo,
				Ordem:       s.Ordem,
				Obrigatoria: s.Obrigatoria,
				Origem:      s.Origem,
				AceitaTeses: s.AceitaTeses,
			})
		}
		dc.ProfileSections = secs
	}
	if i == nil {
		return dc
	}
	dc.IntimationType = i.Type
	// The DJEN intimation teor is stored as HTML — strip it to plain text so the LLM
	// gets clean signal (parties, order, dates) without markup noise wasting tokens.
	dc.IntimationText = stripHTML(i.Content)
	dc.Court = i.Court
	dc.Degree = i.Degree
	dc.Class = i.Class
	dc.Subject = i.Subject
	dc.CNJNumber = i.CNJNumber
	dc.JudgingBody = i.JudgingBody
	dc.DeadlineDate = i.DeadlineEndDate
	// Resolve signing lawyer from the matched OAB recipient (our advogado).
	sl := signingLawyerFromRecipients(i.Recipients)
	dc.SigningLawyerName = sl.Name
	dc.SigningLawyerOAB = sl.OAB
	dc.SigningLawyerUF = sl.UF
	return dc
}

// buildFindings validates and filters the LLM's raw suggestions:
//   - original must appear as a substring in contentForValidation → drop if not
//   - Argumento/Coerência must have a non-empty citation → drop if absent
//   - cap at 10 after filtering
//
// contentForValidation is the current draft text to validate `original` substrings
// against. For Revisar this is the draft's current editor content; Gerar no longer
// calls buildFindings directly.
//
// Returns the validated Finding slice and the Coverage summary.
func buildFindings(suggestions []rawSuggestion, grounded bool, chunksUsed int, contentForValidation string) ([]Finding, Coverage) {
	total := len(suggestions)
	documentsCited := map[string]bool{}

	findings := make([]Finding, 0, min(total, 10))
	for _, s := range suggestions {
		if len(findings) >= 10 {
			break // top-10 cap; the remainder is counted as dropped below
		}
		// Substring validation: original must appear in the current draft content.
		if !strings.Contains(contentForValidation, s.Original) {
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

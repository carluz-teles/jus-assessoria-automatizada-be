package draft

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/eproc"
	"github.com/jusassessoria/platform/lib/httpx"
	"github.com/jusassessoria/platform/lib/pubsub"
)

// handler.go is the draft slice's HTTP surface — POST /v1/pecas, GET /v1/pecas/:id,
// PATCH /v1/pecas/:id. The slice owns its routing; cmd/api only composes by calling
// RegisterV1. tenant_id ALWAYS comes from the verified principal, never the body or
// the path.

// writer is the narrow port the Handler uses from the write use case.
type writer interface {
	Create(ctx context.Context, cmd CreateCommand) (CreateResult, error)
	Patch(ctx context.Context, cmd PatchCommand) (*PatchResult, error)
	GetDetail(ctx context.Context, tenantID, draftID string) (*DraftDetailView, error)
	AttachDocument(ctx context.Context, cmd AttachDocumentCommand) (*Attachment, error)
	UpdateAttachmentCategory(ctx context.Context, cmd UpdateAttachmentCategoryCommand) (*Attachment, error)
	RemoveAttachment(ctx context.Context, cmd RemoveAttachmentCommand) error
	Sign(ctx context.Context, cmd SignCommand) (*SignResult, error)
	File(ctx context.Context, cmd FileCommand) (*FileResult, error)
	Result(ctx context.Context, cmd ResultCommand) (*ResultResult, error)
	// Workflow steps (Fatia 2a) — apenas gestos, sem lógica de LLM/outbox.
	SendToSigning(ctx context.Context, tenantID, draftID string) error
	RevertToConstruction(ctx context.Context, tenantID, draftID string) error
	// SaveContentHtml (Fase B do editor rico) grava o HTML do Tiptap.
	SaveContentHtml(ctx context.Context, tenantID, draftID, html string) error
	// AssumeAuthorship (Peça v2) is exposed via the main UseCase — no separate
	// wiring needed since it's a single UPDATE (no LLM, outbox).
	AssumeAuthorship(ctx context.Context, tenantID, draftID string) (*Draft, error)
	// ── Peticionamento automático (Fatia 1 — e-SAJ) ──
	ApproveFiling(ctx context.Context, cmd ApproveFilingCommand) (*ApproveFilingResult, error)
	GetFilingStatus(ctx context.Context, tenantID, draftID string) (*FilingAttempt, error)
	UploadEsajCredential(ctx context.Context, cmd UploadEsajCredentialCommand) (*EsajCredential, error)
	ListEsajCredentials(ctx context.Context, tenantID string) ([]EsajCredential, error)
	RevokeEsajCredential(ctx context.Context, tenantID, id string) error
}

// generator is the narrow port for the POST /v1/pecas/:id/generate trigger. It is a
// separate interface (not embedded in writer) because the generate use case is composed
// independently (different dependencies: outbox, UoW).
type generator interface {
	TriggerGeneration(ctx context.Context, cmd TriggerGenerationCommand) (*Draft, error)
}

// chatter is the narrow port for the chat endpoints (POST + GET /v1/pecas/:id/chat).
// Composed independently (different dep set: no outbox, synchronous LLM call).
type chatter interface {
	AnswerQuestion(ctx context.Context, cmd AnswerQuestionCommand) (*ChatMessage, error)
	GetThread(ctx context.Context, tenantID, draftID string) ([]ChatMessage, *Draft, error)
}

// reviewer is the narrow port for POST /v1/pecas/:id/review (Revisar síncrono).
// Composed independently from the generator and the chat use case.
type reviewer interface {
	ReviewDraft(ctx context.Context, cmd ReviewDraftCommand) (*ReviewResult, error)
}

// thesisSuggester is the narrow port for POST /v1/pecas/:id/theses (Sugerir Teses
// síncrono, Fatia 5). Composed independently — it is stateless (no writer).
type thesisSuggester interface {
	SuggestTheses(ctx context.Context, cmd SuggestThesesCommand) (*SuggestThesesResult, error)
}

// draftThesesStore is the narrow port for the PERSISTED thesis endpoints (Sugerir
// Teses persistido, C1): GET (list persisted), POST (regenerate+persist), PATCH
// (update selection state). Composed independently; requires AI config for POST.
type draftThesesStore interface {
	GenerateDraftTheses(ctx context.Context, tenantID, draftID string) ([]SuggestedThesis, error)
	ListDraftTheses(ctx context.Context, tenantID, draftID string) ([]SuggestedThesis, error)
	UpdateThesisState(ctx context.Context, tenantID, thesisID, state string) (*SuggestedThesis, error)
	// Fluxo da partida (C2): teses persistidas intimation-scoped.
	GenerateIntimationTheses(ctx context.Context, tenantID, intimationID string) ([]SuggestedThesis, error)
	ListIntimationTheses(ctx context.Context, tenantID, intimationID string) ([]SuggestedThesis, error)
}

// iterator is the narrow port for POST /v1/pecas/:id/iterate (Peça v2). Composed
// independently — it is stateless (no writer; the FE applies changes via PATCH).
type iterator interface {
	Iterate(ctx context.Context, cmd IterateCommand) (*IterateResult, error)
}

// lister is the narrow port for the paginated list endpoints.
type lister interface {
	ListByProcess(ctx context.Context, q ListByProcessQuery) (DraftListResult, error)
	ListAll(ctx context.Context, q ListAllQuery) (DraftListResult, error)
}

// presigner is the narrow port for presigned URL generation (export endpoint).
type presigner interface {
	Export(ctx context.Context, cmd ExportCommand) (*ExportResult, error)
}

// Handler is the draft HTTP surface. It owns its routing; cmd/api composes via
// RegisterV1.
type Handler struct {
	uc           writer
	gen          generator        // nil when the generation use case is not wired (no AI key)
	chat         chatter          // nil when the chat use case is not wired
	review       reviewer         // nil when the review use case is not wired
	theses       thesisSuggester  // nil when the theses use case is not wired (no AI key)
	thesesStore  draftThesesStore // nil when the persisted-theses use case is not wired
	lister       lister           // nil when the list use case is not wired
	export       presigner        // nil when the export use case is not wired
	iter         iterator         // nil when the iterate use case is not wired
	storage      StoragePresigner // nil quando storage não configurado — download do PDF fica indisponível
	chunkSub     chunkSubscriber  // nil → SSE stream não montado
	getSaga      getSagaFn        // reader minimal pra saga_state
	streamTokens StreamTokenStore // nil → issuer de stream-token não habilitado
}

// chunkSubscriber é a porta narrow que o SSE precisa pra ler entries do
// Redis Stream do draft. lastID vazio = replay do começo; ID do último
// entry conhecido = retoma dali. Satisfeita por lib/pubsub.StreamSubscriber.
type chunkSubscriber interface {
	XSubscribe(ctx context.Context, streamKey string, lastID string) (<-chan pubsub.StreamMessage, error)
}

// getSagaFn devolve o saga_state atual pra o SSE poder terminar quando a
// geração já concluiu (evita stream infinito depois do DRAFTED/FAILED).
type getSagaFn func(ctx context.Context, tenantID, draftID string) (string, error)

// NewHandler wires the handler to the use case.
func NewHandler(uc writer) *Handler {
	return &Handler{uc: uc}
}

// WithGenerator attaches the generation trigger use case to the handler. Called by
// cmd/api composition when the generation use case is available.
func (h *Handler) WithGenerator(gen generator) *Handler {
	h.gen = gen
	return h
}

// WithGenerationStream ativa o endpoint SSE de streaming da geração. Requer
// tanto o Subscriber (pra ler chunks do canal Redis) quanto o getSaga
// (pra saber quando fechar). Ambos vêm do api composition.
func (h *Handler) WithGenerationStream(sub chunkSubscriber, getSaga getSagaFn) *Handler {
	h.chunkSub = sub
	h.getSaga = getSaga
	return h
}

// WithChat attaches the chat use case to the handler. Called by cmd/api composition
// when the chat use case is available (requires AI config).
func (h *Handler) WithChat(chat chatter) *Handler {
	h.chat = chat
	return h
}

// WithReviewer attaches the review use case to the handler. Called by cmd/api composition
// when the review use case is available (requires AI config).
func (h *Handler) WithReviewer(rev reviewer) *Handler {
	h.review = rev
	return h
}

// WithTheses attaches the thesis-suggestion use case to the handler. Called by
// cmd/api composition when the theses use case is available (requires AI config).
func (h *Handler) WithTheses(t thesisSuggester) *Handler {
	h.theses = t
	return h
}

// WithThesesStore attaches the persisted-theses use case (Sugerir Teses persistido,
// C1) to the handler. Called by cmd/api composition when the theses use case is
// available (requires AI config for the POST/regenerate path).
func (h *Handler) WithThesesStore(s draftThesesStore) *Handler {
	h.thesesStore = s
	return h
}

// WithLister attaches the list use case to the handler.
func (h *Handler) WithLister(l lister) *Handler {
	h.lister = l
	return h
}

// WithStorage anexa o presigner do object storage. Necessário pra gerar
// signed_pdf_url no read model (Fatia 2b). Sem storage, o campo fica null.
func (h *Handler) WithStorage(s StoragePresigner) *Handler {
	h.storage = s
	return h
}

// WithExport attaches the export use case to the handler.
func (h *Handler) WithExport(e presigner) *Handler {
	h.export = e
	return h
}

// WithIterator attaches the iterate use case (Peça v2). Called by cmd/api when
// the LLM is configured.
func (h *Handler) WithIterator(i iterator) *Handler {
	h.iter = i
	return h
}

// RegisterV1 mounts the peças routes on the /v1 group. The static-vs-param ordering
// matters: /pecas/:id/anexos must be declared before /pecas/:id so Fiber routes them
// correctly. Fiber's router is declaration-order-sensitive for sub-resources under a
// param segment.
func (h *Handler) RegisterV1(r fiber.Router) {
	r.Post("/pecas", h.createPeca)
	r.Get("/pecas", h.listPecas)
	r.Get("/pecas/:id", h.getPeca)
	r.Patch("/pecas/:id", h.patchPeca)

	// Peticionamento (Fatia 4).
	r.Post("/pecas/:id/sign", h.signPeca)
	r.Post("/pecas/:id/file", h.filePeca)
	r.Patch("/pecas/:id/result", h.resultPeca)
	r.Get("/pecas/:id/export", h.exportPeca)

	// Peticionamento automático (Fatia 1 — e-SAJ RPA).
	r.Post("/pecas/:id/filing/approve", h.approveFiling)
	r.Get("/pecas/:id/filing", h.getFilingStatus)
	r.Post("/esaj-credentials", h.uploadEsajCredential)
	r.Get("/esaj-credentials", h.listEsajCredentials)
	r.Delete("/esaj-credentials/:id", h.revokeEsajCredential)

	// Workflow steps (Fatia 2a — persistir "onde parei" do peticionamento).
	// sent_to_signing_at é o gesto de avançar Construção → Assinatura;
	// revert nulla o mesmo (só permite se ainda não assinado).
	r.Post("/pecas/:id/enviar-para-assinatura", h.sendToSigning)
	r.Post("/pecas/:id/voltar-para-construcao", h.revertToConstruction)

	// Editor rico (Fase B) — autosave do HTML do Tiptap.
	r.Put("/pecas/:id/content-html", h.saveContentHtml)

	// Streaming da geração (Fatia 2 do streaming). SSE: cliente conecta
	// durante EXTRACTING e recebe chunks do LLM em tempo real. Fecha
	// quando saga_state=DRAFTED/FAILED. Auth alternativa via
	// ?stream_token=xxx (curto, opaco, gerado pelo POST /stream-token).
	r.Post("/pecas/:id/stream-token", h.issueStreamToken)
	r.Get("/pecas/:id/generation-stream", h.generationStream)

	// Process-scoped list.
	r.Get("/processos/:id/pecas", h.listPecasByProcess)

	// AI generation trigger (Fatia 3).
	r.Post("/pecas/:id/generate", h.generatePeca)

	// AI review trigger (Fatia 3 — Revisar síncrono).
	r.Post("/pecas/:id/review", h.reviewPeca)

	// AI thesis suggestion (Sugerir Teses persistido, C1). GET lê as teses
	// persistidas; POST regenera (delete+gera+persiste) e devolve as persistidas;
	// PATCH atualiza o estado de seleção de uma tese. A rota GET migrou pra cá do
	// slice `thesis` (era listThesesByDraft, desacoplado/vazio) — agora serve a
	// lista real do draft.
	r.Get("/pecas/:id/theses", h.listPecaTheses)
	r.Post("/pecas/:id/theses", h.thesesPeca)
	r.Patch("/pecas/:id/theses/:thesisId", h.patchThesisState)
	// Fluxo da PARTIDA (C2): teses PERSISTIDAS ligadas à intimação, antes do draft
	// existir (tela /pecas/nova — evita criar draft zumbi a cada clique). GET lê as
	// persistidas; POST regenera (delete+gera+persiste). Na promoção partida→
	// construção (createDraft) as teses + a seleção são copiadas pro draft. Substitui
	// a antiga POST /v1/theses stateless (removida — nada mais a consome).
	r.Get("/intimacoes/:id/theses", h.listIntimationTheses)
	r.Post("/intimacoes/:id/theses", h.thesesFromIntimation)

	// Iteração + assumir autoria (Peça v2).
	r.Post("/pecas/:id/iterate", h.iteratePeca)
	r.Post("/pecas/:id/assume-authorship", h.assumeAuthorship)

	// Grounded chat (Fatia 3b).
	r.Post("/pecas/:id/chat", h.postChat)
	r.Get("/pecas/:id/chat", h.getChat)

	// Attachment sub-resource (Fatia 2).
	r.Post("/pecas/:id/anexos", h.attachDocument)
	r.Patch("/pecas/:id/anexos/:attachmentId", h.updateAttachmentCategory)
	r.Delete("/pecas/:id/anexos/:attachmentId", h.removeAttachment)
}

// ─── POST /v1/pecas ──────────────────────────────────────────────────────────

// createPeca handles POST /v1/pecas. Returns 201 on first creation, 200 on
// idempotent (same tenant+intimation_id already has a draft).
func (h *Handler) createPeca(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	// UserID vem do principal — usado como created_by da peça pra o read model
	// da lista mostrar o autor. Pode ser vazio se a rota estiver sob AuthUser
	// (não é o caso hoje pra /pecas, mas o repo tolera vazio → NULL).
	var createdBy string
	if p, ok := httpx.PrincipalFromCtx(c); ok {
		createdBy = p.UserID
	}

	var req CreateRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteError(c, apperr.NewInvalid("corpo inválido"))
	}
	if err := req.Validate(); err != nil {
		return httpx.WriteValidationError(c, err)
	}

	result, err := h.uc.Create(c.UserContext(), CreateCommand{
		TenantID:     tenantID,
		CreatedBy:    createdBy,
		Source:       req.Source,
		IntimationID: req.IntimationID,
		CaseID:       req.CaseID,
		PieceType:    req.PieceType,
		Title:        req.Title,
		TaskID:       req.TaskID,
		ThesisIDs:    req.ThesisIDs,
	})
	if err != nil {
		return httpx.WriteError(c, err)
	}

	status := fiber.StatusCreated
	if !result.IsNewDraft {
		status = fiber.StatusOK
	}
	return c.Status(status).JSON(fiber.Map{"data": draftToResponse(result.Draft)})
}

// ─── GET /v1/pecas/:id ────────────────────────────────────────────────────────

// getPeca handles GET /v1/pecas/:id.
func (h *Handler) getPeca(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	draftID := c.Params("id")

	view, err := h.uc.GetDetail(c.UserContext(), tenantID, draftID)
	if err != nil {
		return httpx.WriteError(c, err)
	}
	resp := detailToResponse(view)
	// Fatia 2b: se a peça já foi assinada e temos storage, gera presigned URL
	// do PDF. TTL curto (15 min). Erro no presign é degradação silenciosa
	// (deixa nil e o FE mostra "Baixar indisponível").
	if view.SignedPDFKey != "" && h.storage != nil {
		if u, err := h.storage.PresignedGet(c.UserContext(), view.SignedPDFKey, 15*time.Minute); err == nil {
			resp.SignedPDFURL = &u
		}
	}
	return c.JSON(fiber.Map{"data": resp})
}

// ─── POST /v1/pecas/:id/iterate (Peça v2) ───────────────────────────────────

// iteratePeca dispatches the synchronous iteration to the LLM. Body carries
// scope + kind/instruction; response carries the SectionChange list.
func (h *Handler) iteratePeca(c *fiber.Ctx) error {
	if h.iter == nil {
		return httpx.WriteError(c, apperr.NewInvalid("Iteração pela IA não está configurada."))
	}
	tenantID := httpx.TenantFromCtx(c)
	draftID := c.Params("id")

	var req IterateRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteError(c, apperr.NewInvalid("corpo inválido"))
	}
	if err := req.Validate(); err != nil {
		return httpx.WriteValidationError(c, err)
	}

	result, err := h.iter.Iterate(c.UserContext(), IterateCommand{
		TenantID:    tenantID,
		DraftID:     draftID,
		Scope:       IterateScope{Kind: req.Scope.Kind, SectionID: req.Scope.SectionID},
		Kind:        req.Kind,
		Instruction: req.Instruction,
	})
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.JSON(fiber.Map{"data": result})
}

// ─── POST /v1/pecas/:id/assume-authorship (Peça v2) ─────────────────────────

// assumeAuthorship flips the peça to human_taken. Idempotent — a repeat call
// returns the same shape.
func (h *Handler) assumeAuthorship(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	draftID := c.Params("id")

	d, err := h.uc.AssumeAuthorship(c.UserContext(), tenantID, draftID)
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.JSON(fiber.Map{"data": fiber.Map{
		"authorship": d.Authorship,
		"updated_at": d.UpdatedAt.Format(time.RFC3339),
	}})
}

// ─── PATCH /v1/pecas/:id ─────────────────────────────────────────────────────

// patchPeca handles PATCH /v1/pecas/:id (autosave).
func (h *Handler) patchPeca(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	draftID := c.Params("id")

	var req PatchRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteError(c, apperr.NewInvalid("corpo inválido"))
	}
	if err := req.Validate(); err != nil {
		return httpx.WriteValidationError(c, err)
	}

	result, err := h.uc.Patch(c.UserContext(), PatchCommand{
		TenantID:          tenantID,
		DraftID:           draftID,
		Content:           req.Content,
		Title:             req.Title,
		StructuredContent: req.StructuredContent,
	})
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.JSON(fiber.Map{"data": patchToResponse(result)})
}

// ─── Response shapes ─────────────────────────────────────────────────────────

// draftResponse is the POST /v1/pecas response body — shared between the 201 and
// the 200 idempotent path.
type draftResponse struct {
	ID           string  `json:"id"`
	TenantID     string  `json:"tenant_id"`
	CaseID       string  `json:"case_id,omitempty"`
	IntimationID string  `json:"intimation_id,omitempty"`
	PieceType    string  `json:"piece_type"`
	Title        string  `json:"title"`
	Content      *string `json:"content"`
	Status       string  `json:"status"`
	SagaState    string  `json:"saga_state"`
	CreatedAt    string  `json:"created_at"`
	UpdatedAt    string  `json:"updated_at"`
	// TaskID/PieceProfileKey (migration 0088) are non-empty only for a
	// task-sourced peça (POST /v1/pecas {task_id}).
	TaskID          string `json:"task_id,omitempty"`
	PieceProfileKey string `json:"piece_profile_key,omitempty"`
}

func draftToResponse(d *Draft) draftResponse {
	var content *string
	if d.Content != "" {
		c := d.Content
		content = &c
	}
	return draftResponse{
		ID:              d.ID,
		TenantID:        d.TenantID,
		CaseID:          d.CaseID,
		IntimationID:    d.IntimationID,
		PieceType:       d.PieceType,
		Title:           d.Title,
		Content:         content,
		Status:          d.Status,
		SagaState:       d.SagaState,
		CreatedAt:       d.CreatedAt.Format(time.RFC3339),
		UpdatedAt:       d.UpdatedAt.Format(time.RFC3339),
		TaskID:          d.TaskID,
		PieceProfileKey: d.PieceProfileKey,
	}
}

// patchResponse is the PATCH /v1/pecas/:id response body.
type patchResponse struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	UpdatedAt string `json:"updated_at"`
}

func patchToResponse(r *PatchResult) patchResponse {
	return patchResponse{
		ID:        r.ID,
		Title:     r.Title,
		UpdatedAt: r.UpdatedAt.Format(time.RFC3339),
	}
}

// detailResponse is the GET /v1/pecas/:id response body.
type detailResponse struct {
	ID        string `json:"id"`
	PieceType string `json:"piece_type"`
	Title     string `json:"title"`
	Content   string `json:"content"`
	Status    string `json:"status"`
	SagaState string `json:"saga_state"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`

	// Peça v2 (migration 0056) — structured_content is the source of truth for
	// the FE; content stays for legacy/export. authorship drives which panel
	// tab (Iterar vs Revisão) the FE shows.
	StructuredContent *StructuredContent `json:"structured_content"`
	// ContentHTML (Fase B do editor rico) — HTML do Tiptap. null pra peças
	// legacy ou geradas pela IA antes do 1º save do editor humano.
	ContentHTML *string `json:"content_html"`
	// ContentEdited: o content_html foi editado à mão desde a última geração
	// (0096). O FE avisa antes de regerar ao mudar teses.
	ContentEdited bool   `json:"content_edited"`
	Authorship    string `json:"authorship"`

	Intimation  *intimationResponse  `json:"intimation,omitempty"`
	Process     *processResponse     `json:"process,omitempty"`
	Deadline    *deadlineResponse    `json:"deadline,omitempty"`
	Attachments []attachmentResponse `json:"attachments"`
	Providences []Providence         `json:"providences"`
	Parties     []partyResponse      `json:"parties"`
	// ProcessDocuments são os autos do processo (documentos fetchados do
	// court_record) listados na seção "Fundada em" do editor. Sempre array
	// (nunca null → []).
	ProcessDocuments []processDocumentResponse `json:"process_documents"`

	// Workflow steps (Fatia 2a — 0060). Cada timestamp é um fato datado; o FE
	// deriva o step atual (Construção/Assinatura/Protocolo/Concluído). Todos
	// nullable: null = "ainda não aconteceu".
	SentToSigningAt *string `json:"sent_to_signing_at"`
	SignedAt        *string `json:"signed_at"`
	FiledAt         *string `json:"filed_at"`
	FilingNumber    string  `json:"filing_number"`
	// Presigned GET URL do PDF assinado (Fatia 2b). null antes de assinar OU
	// quando o storage não está configurado. TTL curto (15 min) — se o link
	// expirar, o FE faz GET /pecas/:id de novo pra pegar um novo.
	SignedPDFURL *string `json:"signed_pdf_url"`

	// Review is the latest AI review, or null when no generation has been run.
	Review *reviewResponse `json:"review"`

	// Reclassificação (fatia 5, docs §7 questão 4): superseded_at não-null diz ao FE
	// "esta peça foi substituída"; superseded_by_draft_id (quando presente) aponta a
	// peça nova. Ambos null pra toda peça vigente.
	SupersededAt        *string `json:"superseded_at"`
	SupersededByDraftID *string `json:"superseded_by_draft_id"`
}

// reviewResponse is the nested review shape in GET /v1/pecas/:id.
type reviewResponse struct {
	Status      string            `json:"status"`
	GeneratedAt string            `json:"generated_at"`
	Grounded    bool              `json:"grounded"`
	Suggestions []findingResponse `json:"suggestions"`
}

// findingResponse is one suggestion in the review response.
type findingResponse struct {
	N           int               `json:"n"`
	Category    string            `json:"category"`
	Original    string            `json:"original"`
	Replacement string            `json:"replacement"`
	Problem     string            `json:"problem"`
	Description string            `json:"description"`
	Citation    *citationResponse `json:"citation,omitempty"`
}

// citationResponse is a grounding citation in a finding.
type citationResponse struct {
	DocumentID string `json:"document_id"`
	Page       int    `json:"page"`
	Quote      string `json:"quote"`
}

type intimationResponse struct {
	ID              string `json:"id"`
	Type            string `json:"type"`
	Content         string `json:"content"`
	MadeAvailableAt string `json:"made_available_at"`
	DeadlineStartAt string `json:"deadline_start_at"`
}

type processResponse struct {
	CaseID        string   `json:"case_id"`
	CourtRecordID string   `json:"court_record_id"`
	CNJNumber     string   `json:"cnj_number"`
	Court         string   `json:"court"`
	Degree        string   `json:"degree"`
	Class         string   `json:"class"`
	Subject       string   `json:"subject"`
	JudgingBody   string   `json:"judging_body"`
	Plaintiffs    []string `json:"plaintiffs"`
	Defendants    []string `json:"defendants"`
}

type deadlineResponse struct {
	ID       string `json:"id"`
	EndDate  string `json:"end_date"`
	DaysLeft int    `json:"days_left"`
	Status   string `json:"status"`
}

// partyResponse is one party in GET /v1/pecas/:id. role is the raw DB enum
// (PLAINTIFF | DEFENDANT | THIRD_PARTY) — the FE maps to autor/reu/procurador.
// counsels is always an array (empty when the party has no advogado registered).
type partyResponse struct {
	Role string `json:"role"`
	Name string `json:"name"`
	// IsClient is TRUE when this party is the one the escritório represents — an
	// advogado of it carries an OAB the tenant watches. Always present so the FE
	// picks THE client by this flag instead of guessing by role (which marks
	// every DEFENDANT a client when there are 2+ réus). FALSE for all parties
	// when the tenant watches no OAB (FE keeps its role-based fallback then).
	IsClient bool              `json:"is_client"`
	Counsels []counselResponse `json:"counsels"`
}

// counselResponse is one advogado aggregated under a party.
type counselResponse struct {
	Name string `json:"name"`
	OAB  string `json:"oab"`
	UF   string `json:"uf"`
}

func detailToResponse(v *DraftDetailView) detailResponse {
	resp := detailResponse{
		ID:                  v.ID,
		PieceType:           v.PieceType,
		Title:               v.Title,
		Content:             v.Content,
		Status:              v.Status,
		SagaState:           v.SagaState,
		CreatedAt:           v.CreatedAt.Format(time.RFC3339),
		UpdatedAt:           v.UpdatedAt.Format(time.RFC3339),
		StructuredContent:   v.StructuredContent,
		ContentHTML:         v.ContentHtml,
		ContentEdited:       v.ContentEdited,
		Authorship:          v.Authorship,
		SentToSigningAt:     timePtrToRFC3339(v.SentToSigningAt),
		SignedAt:            timePtrToRFC3339(v.SignedAt),
		FiledAt:             timePtrToRFC3339(v.FiledAt),
		FilingNumber:        v.FilingNumber,
		SupersededAt:        timePtrToRFC3339(v.SupersededAt),
		SupersededByDraftID: stringPtrOrNil(v.SupersededByDraftID),
	}

	if v.Intimation != nil {
		resp.Intimation = &intimationResponse{
			ID:              v.Intimation.ID,
			Type:            v.Intimation.Type,
			Content:         v.Intimation.Content,
			MadeAvailableAt: v.Intimation.MadeAvailableAt.Format(time.DateOnly),
			DeadlineStartAt: v.Intimation.DeadlineStartAt.Format(time.DateOnly),
		}
	}

	if v.Process != nil {
		resp.Process = &processResponse{
			CaseID:        v.Process.CaseID,
			CourtRecordID: v.Process.CourtRecordID,
			CNJNumber:     v.Process.CNJNumber,
			Court:         v.Process.Court,
			Degree:        v.Process.Degree,
			Class:         v.Process.Class,
			Subject:       v.Process.Subject,
			JudgingBody:   v.Process.JudgingBody,
			Plaintiffs:    v.Process.Plaintiffs,
			Defendants:    v.Process.Defendants,
		}
	}

	if v.Deadline != nil {
		daysLeft := daysLeftFromNow(v.Deadline.EndDate)
		resp.Deadline = &deadlineResponse{
			ID:       v.Deadline.ID,
			EndDate:  v.Deadline.EndDate.Format(time.DateOnly),
			DaysLeft: daysLeft,
			Status:   v.Deadline.Status,
		}
	}

	// Attachments: always an array (empty when none), never omitted.
	resp.Attachments = attachmentsToResponse(v.Attachments)

	// Providences: always an array (empty when none — drafts without an intimation,
	// or with no tasks yet). Peça v2 FE renders the sidebar bullet list.
	resp.Providences = v.Providences
	if resp.Providences == nil {
		resp.Providences = []Providence{}
	}

	// Parties: always an array (empty when the draft has no process, or the
	// case genuinely has no party materialized). Peça v2 FE renders bloco PARTES.
	resp.Parties = partiesToResponse(v.Parties)

	// ProcessDocuments: autos do processo (court_record) — sempre array (empty
	// quando o draft não tem process ou o processo não tem autos). FE renderiza
	// na seção "Fundada em" do editor.
	resp.ProcessDocuments = processDocumentsToResponse(v.ProcessDocuments)

	// Review: nil when no generation has run yet, otherwise the latest review.
	if v.Review != nil {
		resp.Review = reviewToResponse(v.Review)
	}

	return resp
}

// reviewToResponse maps a *Review entity to the nested response shape.
func reviewToResponse(r *Review) *reviewResponse {
	suggestions := make([]findingResponse, 0, len(r.Findings))
	for _, f := range r.Findings {
		fr := findingResponse{
			N:           f.N,
			Category:    f.Category,
			Original:    f.Original,
			Replacement: f.Replacement,
			Problem:     f.Problem,
			Description: f.Description,
		}
		if f.Citation != nil {
			fr.Citation = &citationResponse{
				DocumentID: f.Citation.DocumentID,
				Page:       f.Citation.Page,
				Quote:      f.Citation.Quote,
			}
		}
		suggestions = append(suggestions, fr)
	}
	return &reviewResponse{
		Status:      r.Status,
		GeneratedAt: r.GeneratedAt.Format(time.RFC3339),
		Grounded:    r.Coverage.Grounded,
		Suggestions: suggestions,
	}
}

// ─── POST /v1/pecas/:id/generate ──────────────────────────────────────────────

// generatePeca handles POST /v1/pecas/:id/generate (Fatia 3). Guards saga_state,
// flips to EXTRACTING, publishes draft.generation_requested in the same tx.
// Returns 202 with {data:{id, saga_state:"EXTRACTING"}}.
//
// The body is entirely OPTIONAL (Fatia 5 — tone/instructions/theses): an empty
// body, or no body at all, is valid and behaves exactly as before Fatia 5
// (backward-compat). A malformed JSON body still fails BodyParser as before.
func (h *Handler) generatePeca(c *fiber.Ctx) error {
	if h.gen == nil {
		// The generation use case was not wired (no AI config). Return 202 with FAILED
		// immediately to match the "generator nil → FAILED" behaviour visible to the FE.
		return c.Status(fiber.StatusAccepted).JSON(fiber.Map{
			"data": fiber.Map{"saga_state": SagaStateFailed, "message": "IA não configurada"},
		})
	}

	tenantID := httpx.TenantFromCtx(c)
	draftID := c.Params("id")

	var req GenerateRequest
	// An empty/absent body is valid (BodyParser leaves req at its zero value on
	// EOF for the JSON content type); only a malformed body errors.
	if len(c.Body()) > 0 {
		if err := c.BodyParser(&req); err != nil {
			return httpx.WriteError(c, apperr.NewInvalid("corpo inválido"))
		}
	}
	if err := req.Validate(); err != nil {
		return httpx.WriteValidationError(c, err)
	}

	draft, err := h.gen.TriggerGeneration(c.UserContext(), TriggerGenerationCommand{
		TenantID:     tenantID,
		DraftID:      draftID,
		Tone:         req.Tone,
		Instructions: req.Instructions,
		Theses:       req.Theses,
	})
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusAccepted).JSON(fiber.Map{
		"data": fiber.Map{
			"id":         draft.ID,
			"saga_state": draft.SagaState,
		},
	})
}

// ─── POST /v1/pecas/:id/review ────────────────────────────────────────────────

// reviewPeca handles POST /v1/pecas/:id/review (Revisar síncrono). Guards reviewer nil,
// tenant, and draftID. Body is intentionally empty (no input needed — the minuta lives in
// the draft row). Returns 200 {data:{review, saga_state:"REVIEWED"}}.
func (h *Handler) reviewPeca(c *fiber.Ctx) error {
	if h.review == nil {
		return httpx.WriteError(c, ErrIANotConfigured)
	}

	tenantID := httpx.TenantFromCtx(c)
	draftID := c.Params("id")

	result, err := h.review.ReviewDraft(c.UserContext(), ReviewDraftCommand{
		TenantID: tenantID,
		DraftID:  draftID,
	})
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.JSON(fiber.Map{
		"data": fiber.Map{
			"review":     reviewToResponse(result.Review),
			"saga_state": result.SagaState,
		},
	})
}

// ─── POST /v1/pecas/:id/theses ────────────────────────────────────────────────

// listPecaTheses handles GET /v1/pecas/:id/theses — the PERSISTED theses of a draft
// (C1). Returns 200 {data:[...]} (empty [] when none generated yet — the FE then
// POSTs to generate). This route migrated from the `thesis` slice (which returned a
// decoupled, always-empty list); it now serves the real draft-scoped rows.
func (h *Handler) listPecaTheses(c *fiber.Ctx) error {
	if h.thesesStore == nil {
		return httpx.WriteError(c, ErrIANotConfigured)
	}
	tenantID := httpx.TenantFromCtx(c)
	draftID := c.Params("id")
	list, err := h.thesesStore.ListDraftTheses(c.UserContext(), tenantID, draftID)
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.JSON(fiber.Map{"data": suggestedThesesToResponse(list)})
}

// thesesPeca handles POST /v1/pecas/:id/theses (Sugerir Teses persistido, C1). It
// ALWAYS regenerates: delete + gera (RAG+LLM) + persiste, returning the persisted
// list. The FE only POSTs on the first visit (GET came back empty) or on an explicit
// "Regenerar" — on revisits it GETs the persisted rows and does NOT regenerate.
// Guards thesesStore nil, tenant, and draftID. Returns 200 {data:[...]}.
func (h *Handler) thesesPeca(c *fiber.Ctx) error {
	if h.thesesStore == nil {
		return httpx.WriteError(c, ErrIANotConfigured)
	}

	tenantID := httpx.TenantFromCtx(c)
	draftID := c.Params("id")

	list, err := h.thesesStore.GenerateDraftTheses(c.UserContext(), tenantID, draftID)
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.JSON(fiber.Map{"data": suggestedThesesToResponse(list)})
}

// patchThesisStateRequest is the body of PATCH /v1/pecas/:id/theses/:thesisId.
type patchThesisStateRequest struct {
	State string `json:"state"`
}

// patchThesisState handles PATCH /v1/pecas/:id/theses/:thesisId — updates one
// persisted thesis's selection state (C1). Body {state}. Validates the enum (400 on
// invalid), 404 on unknown id. Returns 200 {data:{...tese...}}.
func (h *Handler) patchThesisState(c *fiber.Ctx) error {
	if h.thesesStore == nil {
		return httpx.WriteError(c, ErrIANotConfigured)
	}
	tenantID := httpx.TenantFromCtx(c)
	thesisID := c.Params("thesisId")

	var req patchThesisStateRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteError(c, apperr.NewInvalid("corpo inválido"))
	}
	row, err := h.thesesStore.UpdateThesisState(c.UserContext(), tenantID, thesisID, req.State)
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.JSON(fiber.Map{"data": suggestedThesisToResponse(*row)})
}

// ─── GET/POST /v1/intimacoes/:id/theses (partida, C2) ─────────────────────────

// listIntimationTheses handles GET /v1/intimacoes/:id/theses — the PERSISTED theses
// of an intimation (fluxo da partida, antes do draft existir). Same shape as the
// draft-scoped list ({data:[...]}). The FE POSTs on the first visit (GET vazio) and
// GETs on revisits.
func (h *Handler) listIntimationTheses(c *fiber.Ctx) error {
	if h.thesesStore == nil {
		return httpx.WriteError(c, ErrIANotConfigured)
	}
	tenantID := httpx.TenantFromCtx(c)
	intimationID := c.Params("id")

	list, err := h.thesesStore.ListIntimationTheses(c.UserContext(), tenantID, intimationID)
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.JSON(fiber.Map{"data": suggestedThesesToResponse(list)})
}

// thesesFromIntimation handles POST /v1/intimacoes/:id/theses — regenera (delete+
// gera+persiste) as teses da partida e devolve as persistidas. Usado pela tela
// /pecas/nova, que difere a criação do draft até o commit (Gerar/Manual); na
// promoção (createDraft) as teses + a seleção migram pro draft. Guarda thesesStore
// nil. Retorna 200 {data:[...]} — MESMO shape do draft (id/state/position).
func (h *Handler) thesesFromIntimation(c *fiber.Ctx) error {
	if h.thesesStore == nil {
		return httpx.WriteError(c, ErrIANotConfigured)
	}
	tenantID := httpx.TenantFromCtx(c)
	intimationID := c.Params("id")

	list, err := h.thesesStore.GenerateIntimationTheses(c.UserContext(), tenantID, intimationID)
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.JSON(fiber.Map{"data": suggestedThesesToResponse(list)})
}

// thesisResponse is one item in the /theses response. The source_* fields attribute
// the thesis to the autos document its evidence came from (empty when grounded only
// in the teor — the FE then shows the teor as source, the pre-existing behavior). The
// snake_case names match the FE's ThesisAPI (pecas-v2) so no mapper change is needed.
type thesisResponse struct {
	Label            string           `json:"label"`
	Confidence       string           `json:"confidence"`
	Reference        string           `json:"reference"`
	Foundation       string           `json:"foundation"`
	Evidence         []string         `json:"evidence"`
	Grounded         bool             `json:"grounded"`
	SourceDocumentID string           `json:"source_document_id,omitempty"`
	SourceLabel      string           `json:"source_label,omitempty"`
	SourceExcerpt    string           `json:"source_excerpt,omitempty"`
	SourcePage       int              `json:"source_page,omitempty"`
	Anchors          []anchorResponse `json:"anchors"`
}

// anchorResponse is one autos document sustaining a thesis (multi-âncora). The FE
// (Fase 2) consumes the full slice; the singular source_* above mirror the primary.
type anchorResponse struct {
	DocumentID string `json:"document_id,omitempty"`
	Label      string `json:"label,omitempty"`
	Excerpt    string `json:"excerpt,omitempty"`
	Page       int    `json:"page,omitempty"`
	Grounded   bool   `json:"grounded"`
}

// anchorsToResponse maps the entity anchors to their wire form (never null).
func anchorsToResponse(anchors []ThesisAnchor) []anchorResponse {
	out := make([]anchorResponse, 0, len(anchors))
	for _, a := range anchors {
		out = append(out, anchorResponse{
			DocumentID: a.DocumentID,
			Label:      a.Label,
			Excerpt:    a.Excerpt,
			Page:       a.Page,
			Grounded:   a.Grounded,
		})
	}
	return out
}

// segmentResponse is one TRECHO of the generated peça a thesis produced (0095).
// The FE shows it when proposing removal ("em qual mudança isso vai implicar") and
// matches `heading` by text to scroll/highlight the section in the editor.
type segmentResponse struct {
	Heading  string `json:"heading"`
	Conteudo string `json:"conteudo"`
}

// segmentsToResponse maps the entity segments to their wire form (never null).
func segmentsToResponse(segments []ThesisSegment) []segmentResponse {
	out := make([]segmentResponse, 0, len(segments))
	for _, s := range segments {
		out = append(out, segmentResponse{Heading: s.Heading, Conteudo: s.Conteudo})
	}
	return out
}

func thesesToResponse(theses []Thesis) []thesisResponse {
	out := make([]thesisResponse, 0, len(theses))
	for _, t := range theses {
		ev := t.Evidence
		if ev == nil {
			ev = []string{} // wire: nunca null (JSON contract)
		}
		out = append(out, thesisResponse{
			Label:            t.Label,
			Confidence:       t.Confidence,
			Reference:        t.Reference,
			Foundation:       t.Foundation,
			Evidence:         ev,
			Grounded:         t.Grounded,
			SourceDocumentID: t.SourceDocumentID,
			SourceLabel:      t.SourceLabel,
			SourceExcerpt:    t.SourceExcerpt,
			SourcePage:       t.SourcePage,
			Anchors:          anchorsToResponse(t.Anchors),
		})
	}
	return out
}

// suggestedThesisResponse is one item in the PERSISTED theses response (C1). It
// extends thesisResponse with id/state/position — the fields the FE (mapThesisFromApi,
// pecas-v2) needs to select and keep a thesis across revisits. snake_case matches
// the FE's ThesisAPI.
type suggestedThesisResponse struct {
	ID               string           `json:"id"`
	State            string           `json:"state"`
	Position         int              `json:"position"`
	Label            string           `json:"label"`
	Confidence       string           `json:"confidence"`
	Reference        string           `json:"reference"`
	Foundation       string           `json:"foundation"`
	Evidence         []string         `json:"evidence"`
	Grounded         bool             `json:"grounded"`
	SourceDocumentID string           `json:"source_document_id,omitempty"`
	SourceLabel      string           `json:"source_label,omitempty"`
	SourceExcerpt    string            `json:"source_excerpt,omitempty"`
	SourcePage       int               `json:"source_page,omitempty"`
	Anchors          []anchorResponse  `json:"anchors"`
	Segments         []segmentResponse `json:"segments"`
}

func suggestedThesisToResponse(t SuggestedThesis) suggestedThesisResponse {
	ev := t.Evidence
	if ev == nil {
		ev = []string{} // wire: nunca null (JSON contract)
	}
	return suggestedThesisResponse{
		ID:               t.ID,
		State:            t.State,
		Position:         t.Position,
		Label:            t.Label,
		Confidence:       t.Confidence,
		Reference:        t.Reference,
		Foundation:       t.Foundation,
		Evidence:         ev,
		Grounded:         t.Grounded,
		SourceDocumentID: t.SourceDocumentID,
		SourceLabel:      t.SourceLabel,
		SourceExcerpt:    t.SourceExcerpt,
		SourcePage:       t.SourcePage,
		Anchors:          anchorsToResponse(t.Anchors),
		Segments:         segmentsToResponse(t.Segments),
	}
}

func suggestedThesesToResponse(list []SuggestedThesis) []suggestedThesisResponse {
	out := make([]suggestedThesisResponse, 0, len(list))
	for _, t := range list {
		out = append(out, suggestedThesisToResponse(t))
	}
	return out
}

// ── Chat handlers (Fatia 3b) ──────────────────────────────────────────────────

// postChat handles POST /v1/pecas/:id/chat {question}.
// Returns 200 {data: message} with the assistant's turn. When the chat use case is
// not wired (no AI config), returns 422 to match ErrIANotConfigured.
func (h *Handler) postChat(c *fiber.Ctx) error {
	if h.chat == nil {
		return httpx.WriteError(c, ErrIANotConfigured)
	}

	tenantID := httpx.TenantFromCtx(c)
	draftID := c.Params("id")

	var req ChatRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteError(c, apperr.NewInvalid("corpo inválido"))
	}
	if err := req.Validate(); err != nil {
		return httpx.WriteValidationError(c, err)
	}

	msg, err := h.chat.AnswerQuestion(c.UserContext(), AnswerQuestionCommand{
		TenantID: tenantID,
		DraftID:  draftID,
		Question: req.Question,
	})
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.JSON(fiber.Map{"data": chatMessageToResponse(msg)})
}

// getChat handles GET /v1/pecas/:id/chat.
// Returns 200 {data: {messages: [...], grounded_capable: bool}}.
// grounded_capable is true when the draft has a court_record_id (i.e. it has case context
// that can be searched via RAG). Determined from draft.CaseID being non-empty.
func (h *Handler) getChat(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	draftID := c.Params("id")

	// If the chat use case is not wired, we still return the thread (empty), just
	// grounded_capable=false (no AI). If the use case is nil, fall back to the
	// main writer's GetDetail for the draft guard, but simpler: return empty thread.
	if h.chat == nil {
		// Need to tenant-guard the draft; use the main use case's GetDetail.
		if _, err := h.uc.GetDetail(c.UserContext(), tenantID, draftID); err != nil {
			return httpx.WriteError(c, err)
		}
		return c.JSON(fiber.Map{
			"data": fiber.Map{
				"messages":         []chatMessageResponse{},
				"grounded_capable": false,
			},
		})
	}

	messages, draft, err := h.chat.GetThread(c.UserContext(), tenantID, draftID)
	if err != nil {
		return httpx.WriteError(c, err)
	}

	// grounded_capable = the draft has a case_id (implying a court_record exists
	// and the embedder can search the corpus). This tells the FE whether to show
	// the "Ancorado nos autos" indicator in the chat UI.
	groundedCapable := draft.CaseID != ""

	msgResponses := make([]chatMessageResponse, 0, len(messages))
	for i := range messages {
		msgResponses = append(msgResponses, chatMessageToResponse(&messages[i]))
	}

	return c.JSON(fiber.Map{
		"data": fiber.Map{
			"messages":         msgResponses,
			"grounded_capable": groundedCapable,
		},
	})
}

// ── Chat response shapes ──────────────────────────────────────────────────────

// chatMessageResponse is the JSON shape for one chat turn.
type chatMessageResponse struct {
	ID           string             `json:"id"`
	DraftID      string             `json:"draft_id"`
	Role         string             `json:"role"`
	Content      string             `json:"content"`
	Citations    []citationResponse `json:"citations"`
	Grounded     bool               `json:"grounded"`
	ModelVersion string             `json:"model_version,omitempty"`
	CreatedAt    string             `json:"created_at"`
}

func chatMessageToResponse(m *ChatMessage) chatMessageResponse {
	cits := make([]citationResponse, 0, len(m.Citations))
	for _, c := range m.Citations {
		cits = append(cits, citationResponse{
			DocumentID: c.DocumentID,
			Page:       c.Page,
			Quote:      c.Quote,
		})
	}
	return chatMessageResponse{
		ID:           m.ID,
		DraftID:      m.DraftID,
		Role:         m.Role,
		Content:      m.Content,
		Citations:    cits,
		Grounded:     m.Grounded,
		ModelVersion: m.ModelVersion,
		CreatedAt:    m.CreatedAt.Format(time.RFC3339),
	}
}

// ── Attachment response shape ─────────────────────────────────────────────────

// attachmentResponse is the per-item shape inside the attachments array in
// GET /v1/pecas/:id and the standalone body for POST / PATCH /v1/pecas/:id/anexos.
type attachmentResponse struct {
	ID         string `json:"id"`
	DocumentID string `json:"document_id"`
	Name       string `json:"name"`
	Category   string `json:"category"`
	MimeType   string `json:"mime_type"`
	SizeBytes  int64  `json:"size_bytes"`
	Status     string `json:"status"`
	Position   int    `json:"position"`
	CreatedAt  string `json:"created_at"`
}

// processDocumentResponse é um auto do processo (document do court_record) na
// seção "Fundada em" do editor em GET /v1/pecas/:id.
type processDocumentResponse struct {
	ID           string `json:"id"`
	Label        string `json:"label"`
	DocumentType string `json:"document_type"`
	// TypeLabel is the ENRICHED friendly label for document_type (via the eproc code
	// table, HumanizeCode fallback) so the FE renders the type chip without mapping raw
	// codes itself. EventDate is the eproc event date (RFC3339), "" when unknown.
	TypeLabel string `json:"type_label"`
	Pages     int    `json:"pages"`
	Status    string `json:"status"`
	EventDate string `json:"event_date"`
}

func processDocumentsToResponse(docs []ProcessDocument) []processDocumentResponse {
	out := make([]processDocumentResponse, 0, len(docs))
	for i := range docs {
		eventDate := ""
		if !docs[i].EventDate.IsZero() {
			eventDate = docs[i].EventDate.Format(time.RFC3339)
		}
		out = append(out, processDocumentResponse{
			ID:           docs[i].ID,
			Label:        docs[i].Label,
			DocumentType: docs[i].DocumentType,
			TypeLabel:    typeLabelFromCode(docs[i].DocumentType),
			Pages:        docs[i].Pages,
			Status:       docs[i].Status,
			EventDate:    eventDate,
		})
	}
	return out
}

// typeLabelFromCode resolves an eproc document_type code into its enriched friendly
// label (the code table first, a humanized form of the raw code as fallback), so the
// FE shows "Certidão"/"Planilha de cálculo" on the type chip without knowing any code.
// Empty code yields "".
func typeLabelFromCode(code string) string {
	if code == "" {
		return ""
	}
	if label := eproc.DocumentTypeLabel(code); label != "" {
		return label
	}
	return eproc.HumanizeCode(code)
}

func attachmentToResponse(a *Attachment) attachmentResponse {
	return attachmentResponse{
		ID:         a.ID,
		DocumentID: a.DocumentID,
		Name:       a.Name,
		Category:   string(a.Category),
		MimeType:   a.MimeType,
		SizeBytes:  a.SizeBytes,
		Status:     a.Status,
		Position:   a.Position,
		CreatedAt:  a.CreatedAt.Format(time.RFC3339),
	}
}

// partiesToResponse maps []PartyInfo to the wire representation, preserving
// role as-is (FE handles the PLAINTIFF→autor / DEFENDANT→reu mapping). counsels
// is always an array (never nil), so the FE can iterate safely.
func partiesToResponse(parties []PartyInfo) []partyResponse {
	out := make([]partyResponse, 0, len(parties))
	for i := range parties {
		counsels := make([]counselResponse, 0, len(parties[i].Counsels))
		for _, c := range parties[i].Counsels {
			counsels = append(counsels, counselResponse{Name: c.Name, OAB: c.OAB, UF: c.UF})
		}
		out = append(out, partyResponse{
			Role:     parties[i].Role,
			Name:     parties[i].Name,
			IsClient: parties[i].IsClient,
			Counsels: counsels,
		})
	}
	return out
}

func attachmentsToResponse(atts []Attachment) []attachmentResponse {
	out := make([]attachmentResponse, 0, len(atts))
	for i := range atts {
		out = append(out, attachmentResponse{
			ID:         atts[i].ID,
			DocumentID: atts[i].DocumentID,
			Name:       atts[i].Name,
			Category:   string(atts[i].Category),
			MimeType:   atts[i].MimeType,
			SizeBytes:  atts[i].SizeBytes,
			Status:     atts[i].Status,
			Position:   atts[i].Position,
			CreatedAt:  atts[i].CreatedAt.Format(time.RFC3339),
		})
	}
	return out
}

// ── Attachment handlers ───────────────────────────────────────────────────────

// attachDocument handles POST /v1/pecas/:id/anexos.
func (h *Handler) attachDocument(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	draftID := c.Params("id")

	var req AttachDocumentRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteError(c, apperr.NewInvalid("corpo inválido"))
	}
	if err := req.Validate(); err != nil {
		return httpx.WriteValidationError(c, err)
	}

	att, err := h.uc.AttachDocument(c.UserContext(), AttachDocumentCommand{
		TenantID:   tenantID,
		DraftID:    draftID,
		DocumentID: req.DocumentID,
		Category:   AttachmentCategory(req.Category),
	})
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"data": attachmentToResponse(att)})
}

// updateAttachmentCategory handles PATCH /v1/pecas/:id/anexos/:attachmentId.
func (h *Handler) updateAttachmentCategory(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	draftID := c.Params("id")
	attachmentID := c.Params("attachmentId")

	var req UpdateAttachmentCategoryRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteError(c, apperr.NewInvalid("corpo inválido"))
	}
	if err := req.Validate(); err != nil {
		return httpx.WriteValidationError(c, err)
	}

	att, err := h.uc.UpdateAttachmentCategory(c.UserContext(), UpdateAttachmentCategoryCommand{
		TenantID:     tenantID,
		DraftID:      draftID,
		AttachmentID: attachmentID,
		Category:     AttachmentCategory(req.Category),
	})
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.JSON(fiber.Map{"data": attachmentToResponse(att)})
}

// removeAttachment handles DELETE /v1/pecas/:id/anexos/:attachmentId.
func (h *Handler) removeAttachment(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	draftID := c.Params("id")
	attachmentID := c.Params("attachmentId")

	if err := h.uc.RemoveAttachment(c.UserContext(), RemoveAttachmentCommand{
		TenantID:     tenantID,
		DraftID:      draftID,
		AttachmentID: attachmentID,
	}); err != nil {
		return httpx.WriteError(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// daysLeftFromNow computes calendar days remaining until endDate (from today). A
// past date yields 0. This is a simple calendar subtraction — the exact business-day
// countdown lives in lib/calendar and is not needed at read time.
func daysLeftFromNow(endDate time.Time) int {
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	end := time.Date(endDate.Year(), endDate.Month(), endDate.Day(), 0, 0, 0, 0, time.UTC)
	diff := end.Sub(today)
	days := int(math.Ceil(diff.Hours() / 24))
	if days < 0 {
		return 0
	}
	return days
}

// ── Fatia 4 — peticionamento handlers ───────────────────────────────────────

// maxCreatedAt is the descending keyset's first-page cursor: a created_at above
// every real row, so the newest-first scan starts at the top.
const maxCreatedAt = "9999-12-31T23:59:59.999999Z"

// maxUUID is the descending keyset's first-page id sentinel: the highest possible
// UUID, so the scan starts at the top.
const maxUUID = "ffffffff-ffff-ffff-ffff-ffffffffffff"

// signPeca handles POST /v1/pecas/:id/sign. Fatia 2b: recebe body
// {certificate_id} pra escolher qual cert do tenant vai assinar. Retorna
// 200 {data:{id, status, signed_at}}.
func (h *Handler) signPeca(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	draftID := c.Params("id")

	var req struct {
		CertificateID string `json:"certificate_id"`
	}
	// Body opcional só pra idempotência (draft já SIGNED continua funcionando
	// mesmo sem cert_id); o UseCase valida cert_id quando for necessário.
	_ = c.BodyParser(&req)

	result, err := h.uc.Sign(c.UserContext(), SignCommand{
		TenantID:      tenantID,
		DraftID:       draftID,
		CertificateID: req.CertificateID,
	})
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.JSON(fiber.Map{
		"data": signResponse{
			ID:       result.ID,
			Status:   result.Status,
			SignedAt: result.SignedAt.Format(time.RFC3339),
		},
	})
}

// filePeca handles POST /v1/pecas/:id/file. Validates the body, resolves
// court_record_id, creates the petition, and returns 201 {data:{...}}.
func (h *Handler) filePeca(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	draftID := c.Params("id")

	var req FileRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteError(c, apperr.NewInvalid("corpo inválido"))
	}
	if err := req.Validate(); err != nil {
		return httpx.WriteValidationError(c, err)
	}

	var filedAt *time.Time
	if req.FiledAt != "" {
		t, err := time.Parse(time.RFC3339, req.FiledAt)
		if err != nil {
			return httpx.WriteError(c, apperr.NewInvalid("invalid filed_at timestamp"))
		}
		filedAt = &t
	}

	result, err := h.uc.File(c.UserContext(), FileCommand{
		TenantID:      tenantID,
		DraftID:       draftID,
		Receipt:       req.Receipt,
		CourtRecordID: req.CourtRecordID,
		FiledAt:       filedAt,
		FilingNumber:  req.FilingNumber,
	})
	if err != nil {
		return httpx.WriteError(c, err)
	}

	status := fiber.StatusCreated
	if result.IsIdempotent {
		status = fiber.StatusOK
	}
	return c.Status(status).JSON(fiber.Map{
		"data": fileResponse{
			PetitionID: result.PetitionID,
			DraftID:    result.DraftID,
			FiledAt:    result.FiledAt.Format(time.RFC3339),
			Receipt:    result.Receipt,
		},
	})
}

// resultPeca handles PATCH /v1/pecas/:id/result. Validates the body and updates
// the petition's observed_result. Returns 200 {data:{petition_id, observed_result}}.
func (h *Handler) resultPeca(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	draftID := c.Params("id")

	var req ResultRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteError(c, apperr.NewInvalid("corpo inválido"))
	}
	if err := req.Validate(); err != nil {
		return httpx.WriteValidationError(c, err)
	}

	result, err := h.uc.Result(c.UserContext(), ResultCommand{
		TenantID:       tenantID,
		DraftID:        draftID,
		ObservedResult: req.ObservedResult,
	})
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.JSON(fiber.Map{
		"data": resultResponse{
			PetitionID:     result.PetitionID,
			ObservedResult: result.ObservedResult,
		},
	})
}

// ─── Peticionamento automático (Fatia 1 — e-SAJ) ─────────────────────────────

// approveFiling handles POST /v1/pecas/:id/filing/approve. NUNCA auto-file sem
// este clique. Guarda SIGNED + credencial ativa (consentimento), congela o PDF
// assinado (snapshot) e enfileira filing.enqueued. Duplo-clique → 200 idempotente
// (mesma tentativa). Retorna 201 na 1ª vez, 200 se já estava enfileirado.
func (h *Handler) approveFiling(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	draftID := c.Params("id")
	userID := ""
	if p, ok := httpx.PrincipalFromCtx(c); ok {
		userID = p.UserID
	}

	result, err := h.uc.ApproveFiling(c.UserContext(), ApproveFilingCommand{
		TenantID: tenantID,
		DraftID:  draftID,
		UserID:   userID,
	})
	if err != nil {
		return httpx.WriteError(c, err)
	}
	status := fiber.StatusCreated
	if result.IsIdempotent {
		status = fiber.StatusOK
	}
	return c.Status(status).JSON(fiber.Map{
		"data": filingApproveResponse{
			FilingAttemptID: result.FilingAttemptID,
			Status:          result.Status,
			IsIdempotent:    result.IsIdempotent,
		},
	})
}

// getFilingStatus handles GET /v1/pecas/:id/filing. Devolve a tentativa ativa de
// protocolo (ou 200 {data:null} se ainda não iniciado).
func (h *Handler) getFilingStatus(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	draftID := c.Params("id")

	attempt, err := h.uc.GetFilingStatus(c.UserContext(), tenantID, draftID)
	if err != nil {
		if errors.Is(err, ErrFilingAttemptNotFound) {
			return c.JSON(fiber.Map{"data": nil})
		}
		return httpx.WriteError(c, err)
	}
	var finishedAt *time.Time
	if !attempt.FinishedAt.IsZero() {
		finishedAt = &attempt.FinishedAt
	}
	return c.JSON(fiber.Map{"data": filingStatusResponse{
		ID:            attempt.ID,
		DraftID:       attempt.DraftID,
		Status:        attempt.Status,
		RequestedAt:   attempt.RequestedAt,
		FinishedAt:    finishedAt,
		FailureReason: attempt.FailureReason,
		FilingNumber:  attempt.FilingNumber,
	}})
}

// uploadEsajCredential handles POST /v1/esaj-credentials. Cadastra login + senha
// (cifrada no envelope KMS) + consentimento dos termos. Owner = principal.
func (h *Handler) uploadEsajCredential(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	userID := ""
	userName := ""
	if p, ok := httpx.PrincipalFromCtx(c); ok {
		userID = p.UserID
		userName = p.UserID // usado só p/ log; o nome real vem do join no list
	}
	_ = userName

	var req struct {
		Login        string `json:"login"`
		Password     string `json:"password"`
		TermsVersion string `json:"terms_version"`
	}
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteError(c, apperr.NewInvalid("malformed request body"))
	}
	if req.Login == "" || req.Password == "" || req.TermsVersion == "" {
		return httpx.WriteError(c, apperr.NewInvalid("login, password e terms_version são obrigatórios"))
	}

	cred, err := h.uc.UploadEsajCredential(c.UserContext(), UploadEsajCredentialCommand{
		TenantID:        tenantID,
		OwnerUserID:     userID,
		Login:           req.Login,
		Password:        req.Password,
		TermsVersion:    req.TermsVersion,
		TermsAcceptedAt: time.Now(),
		TermsAcceptedBy: userID,
	})
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{
		"data": esajCredentialResponse(cred),
	})
}

// listEsajCredentials handles GET /v1/esaj-credentials → {data: [...]}.
func (h *Handler) listEsajCredentials(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	items, err := h.uc.ListEsajCredentials(c.UserContext(), tenantID)
	if err != nil {
		return httpx.WriteError(c, err)
	}
	views := make([]esajCredentialView, 0, len(items))
	for i := range items {
		views = append(views, esajCredentialResponse(&items[i]))
	}
	return c.JSON(fiber.Map{"data": views})
}

// revokeEsajCredential handles DELETE /v1/esaj-credentials/:id → 204.
func (h *Handler) revokeEsajCredential(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	id := c.Params("id")
	if err := h.uc.RevokeEsajCredential(c.UserContext(), tenantID, id); err != nil {
		return httpx.WriteError(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// ── response DTOs (e-SAJ + filing) ───────────────────────────────────────────

type filingApproveResponse struct {
	FilingAttemptID string `json:"filing_attempt_id"`
	Status          string `json:"status"`
	IsIdempotent    bool   `json:"is_idempotent"`
}

type filingStatusResponse struct {
	ID            string     `json:"id"`
	DraftID       string     `json:"draft_id"`
	Status        string     `json:"status"`
	RequestedAt   time.Time  `json:"requested_at"`
	FinishedAt    *time.Time `json:"finished_at,omitempty"`
	FailureReason string     `json:"failure_reason,omitempty"`
	FilingNumber  string     `json:"filing_number,omitempty"`
}

type esajCredentialView struct {
	ID              string `json:"id"`
	OwnerUserID     string `json:"owner_user_id"`
	Login           string `json:"login"`
	TermsVersion    string `json:"terms_version"`
	TermsAcceptedAt string `json:"terms_accepted_at"`
	CreatedAt       string `json:"created_at"`
}

func esajCredentialResponse(c *EsajCredential) esajCredentialView {
	return esajCredentialView{
		ID:              c.ID,
		OwnerUserID:     c.OwnerUserID,
		Login:           c.Login,
		TermsVersion:    c.TermsVersion,
		TermsAcceptedAt: c.TermsAcceptedAt.Format(time.RFC3339),
		CreatedAt:       c.CreatedAt.Format(time.RFC3339),
	}
}

// exportPeca handles GET /v1/pecas/:id/export?format=docx|pdf. Returns 200
// {data:{url, expires_in}} with a presigned download URL.
func (h *Handler) exportPeca(c *fiber.Ctx) error {
	if h.export == nil {
		return httpx.WriteError(c, apperr.NewInvalid("export not configured"))
	}

	tenantID := httpx.TenantFromCtx(c)
	draftID := c.Params("id")
	format := c.Query("format")

	if format != "docx" && format != "pdf" {
		return httpx.WriteError(c, ErrExportFormatInvalid)
	}

	result, err := h.export.Export(c.UserContext(), ExportCommand{
		TenantID: tenantID,
		DraftID:  draftID,
		Format:   format,
	})
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.JSON(fiber.Map{
		"data": exportResponse{
			URL:       result.URL,
			ExpiresIn: result.ExpiresIn,
		},
	})
}

// listPecasByProcess handles GET /v1/processos/:id/pecas. Returns a paginated
// list of peças for a given process (case_id).
func (h *Handler) listPecasByProcess(c *fiber.Ctx) error {
	if h.lister == nil {
		return httpx.WriteError(c, apperr.NewInvalid("list not configured"))
	}

	tenantID := httpx.TenantFromCtx(c)
	caseID := c.Params("id")
	limit := httpx.ClampLimit(c.QueryInt("limit"), httpx.DefaultLimit, httpx.MaxLimit)

	lastCreated, lastID := maxCreatedAt, maxUUID
	if tok := c.Query("cursor"); tok != "" {
		cur, err := httpx.DecodeCursor(tok)
		if err != nil {
			return httpx.WriteError(c, err)
		}
		lastCreated, lastID = cur.LastSortValue, cur.LastID
	}

	res, err := h.lister.ListByProcess(c.UserContext(), ListByProcessQuery{
		TenantID:    tenantID,
		CaseID:      caseID,
		LastCreated: lastCreated,
		LastID:      lastID,
		Limit:       limit,
	})
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(newDraftListPage(res, limit))
}

// listPecas handles GET /v1/pecas (tenant library). Returns a paginated list of
// all peças for the tenant, with optional filters.
func (h *Handler) listPecas(c *fiber.Ctx) error {
	if h.lister == nil {
		return httpx.WriteError(c, apperr.NewInvalid("list not configured"))
	}

	tenantID := httpx.TenantFromCtx(c)
	limit := httpx.ClampLimit(c.QueryInt("limit"), httpx.DefaultLimit, httpx.MaxLimit)
	pieceType := c.Query("piece_type")
	status := c.Query("status")
	// workflow_state: chip "Aguardando assinatura"/"Aguardando protocolo"
	// urgencia: chip "Prazo em atraso"/"Prazo hoje" (contra deadline da intimation)
	workflowState := c.Query("workflow_state")
	urgencia := c.Query("urgencia")

	var principalUserID string
	if p, ok := httpx.PrincipalFromCtx(c); ok {
		principalUserID = p.UserID
	}
	assignee, err := resolveAssignee(c.Query("assignee"), principalUserID)
	if err != nil {
		return httpx.WriteError(c, err)
	}

	lastCreated, lastID := maxCreatedAt, maxUUID
	if tok := c.Query("cursor"); tok != "" {
		cur, err := httpx.DecodeCursor(tok)
		if err != nil {
			return httpx.WriteError(c, err)
		}
		lastCreated, lastID = cur.LastSortValue, cur.LastID
	}

	res, err := h.lister.ListAll(c.UserContext(), ListAllQuery{
		TenantID:      tenantID,
		PieceType:     pieceType,
		Status:        status,
		WorkflowState: workflowState,
		Urgencia:      urgencia,
		Assignee:      assignee,
		LastCreated:   lastCreated,
		LastID:        lastID,
		Limit:         limit,
	})
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(newDraftListPage(res, limit))
}

// resolveAssignee turns the ?assignee filter (chip "Minhas") into the value ListAll
// filters d.created_by on: "" is no filter, the sentinel "me" resolves to the
// principal's own id, and any other value must be a well-formed uuid (a client error
// → 400 otherwise). Mirrors deadline's resolveAssignee (internal/deadline/handler.go)
// and acquisition's resolveIntimacaoAssignee — kept as a local copy since slices only
// communicate by event, never by importing each other's code.
func resolveAssignee(assignee, principalUserID string) (string, error) {
	switch assignee {
	case "":
		return "", nil
	case "me":
		return principalUserID, nil
	default:
		if _, err := uuid.Parse(assignee); err != nil {
			return "", apperr.NewInvalid("invalid assignee filter (want a user id or \"me\")")
		}
		return assignee, nil
	}
}

// ── Fatia 4 response shapes ────────────────────────────────────────────────

type signResponse struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	SignedAt string `json:"signed_at"`
}

type fileResponse struct {
	PetitionID string         `json:"petition_id"`
	DraftID    string         `json:"draft_id"`
	FiledAt    string         `json:"filed_at"`
	Receipt    map[string]any `json:"receipt"`
}

type resultResponse struct {
	PetitionID     string `json:"petition_id"`
	ObservedResult string `json:"observed_result"`
}

type exportResponse struct {
	URL       string `json:"url"`
	ExpiresIn int    `json:"expires_in"`
}

// draftListItemResponse is the per-item shape in paginated peça lists.
type draftListItemResponse struct {
	ID              string            `json:"id"`
	PieceType       string            `json:"piece_type"`
	Title           string            `json:"title"`
	Status          string            `json:"status"`
	SagaState       string            `json:"saga_state"`
	CoverageSummary *coverageSummaryR `json:"coverage_summary,omitempty"`
	SentToSigningAt *string           `json:"sent_to_signing_at,omitempty"`
	SignedAt        *string           `json:"signed_at,omitempty"`
	FiledAt         *string           `json:"filed_at,omitempty"`
	ObservedResult  *string           `json:"observed_result,omitempty"`
	CreatedAt       string            `json:"created_at"`
	// Contexto do processo pra card sem 2ª chamada.
	CNJNumber string `json:"cnj_number,omitempty"`
	// Nome do autor da peça — vazio quando o draft não tem created_by
	// (peça pré-0063 ou usuário removido do escritório).
	ResponsibleName string `json:"responsible_name,omitempty"`
	// Prazo da intimação de origem — nil quando não há deadline derivado.
	DeadlineEndDate  *string `json:"deadline_end_date,omitempty"`
	DeadlineDaysLeft *int32  `json:"deadline_days_left,omitempty"`
}

type coverageSummaryR struct {
	Grounded         bool `json:"grounded"`
	ChunksUsed       int  `json:"chunks_used"`
	SuggestionsTotal int  `json:"suggestions_total"`
}

// newDraftListPage wraps the list read model in the cursor envelope.
func newDraftListPage(res DraftListResult, limit int) httpx.Page[draftListItemResponse] {
	items := make([]draftListItemResponse, 0, len(res.Items))
	for _, it := range res.Items {
		resp := draftListItemResponse{
			ID:              it.ID,
			PieceType:       it.PieceType,
			Title:           it.Title,
			Status:          it.Status,
			SagaState:       it.SagaState,
			CreatedAt:       it.CreatedAt.Format(time.RFC3339),
			CNJNumber:       it.CNJNumber,
			ResponsibleName: it.ResponsibleName,
		}
		if it.CoverageSummary != nil {
			resp.CoverageSummary = &coverageSummaryR{
				Grounded:         it.CoverageSummary.Grounded,
				ChunksUsed:       it.CoverageSummary.ChunksUsed,
				SuggestionsTotal: it.CoverageSummary.SuggestionsTotal,
			}
		}
		if it.SentToSigningAt != nil {
			s := it.SentToSigningAt.Format(time.RFC3339)
			resp.SentToSigningAt = &s
		}
		if it.SignedAt != nil {
			s := it.SignedAt.Format(time.RFC3339)
			resp.SignedAt = &s
		}
		if it.FiledAt != nil {
			s := it.FiledAt.Format(time.RFC3339)
			resp.FiledAt = &s
		}
		if it.DeadlineEndDate != nil {
			s := it.DeadlineEndDate.Format("2006-01-02")
			resp.DeadlineEndDate = &s
			resp.DeadlineDaysLeft = it.DeadlineDaysLeft
		}
		if it.ObservedResult != nil {
			resp.ObservedResult = it.ObservedResult
		}
		items = append(items, resp)
	}

	meta := httpx.PageMeta{Limit: limit, TotalCount: res.Total, Total: res.Total}
	if res.HasMore && len(items) > 0 {
		last := res.Items[len(items)-1]
		tok := httpx.EncodeCursor(httpx.Cursor{
			LastID:        last.ID,
			LastSortValue: last.CreatedAt.UTC().Format(time.RFC3339Nano),
		})
		meta.NextCursor = &tok
	}
	return httpx.Page[draftListItemResponse]{Data: items, Page: meta}
}

// timePtrToRFC3339 formats a *time.Time as *string in RFC3339. nil in → nil out.
func timePtrToRFC3339(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.Format(time.RFC3339)
	return &s
}

// stringPtrOrNil lifts an entity's "" == absent convention to a JSON-nullable *string —
// used for SupersededByDraftID (fatia 5), where "" means "not linked yet" and must render
// as null, never an empty string, to the FE.
func stringPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// sendToSigning handles POST /v1/pecas/:id/enviar-para-assinatura. Marca o
// gesto "usuário terminou Construção e passou pra Assinatura". Idempotente:
// já enviado devolve 200. Body vazio.
func (h *Handler) sendToSigning(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	draftID := c.Params("id")
	if err := h.uc.SendToSigning(c.UserContext(), tenantID, draftID); err != nil {
		return httpx.WriteError(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// saveContentHtml handles PUT /v1/pecas/:id/content-html. Autosave do editor
// rico (Fase B). Body: {"content_html": "<p>..."}. 200 vazio quando ok.
// Limite defensivo: 500 KB de HTML — peças jurídicas raramente ultrapassam.
func (h *Handler) saveContentHtml(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	draftID := c.Params("id")
	var req struct {
		ContentHTML string `json:"content_html"`
	}
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteError(c, apperr.NewInvalid("corpo inválido"))
	}
	if len(req.ContentHTML) > 500_000 {
		return httpx.WriteError(c, apperr.NewInvalid("content_html excede 500 KB"))
	}
	if err := h.uc.SaveContentHtml(c.UserContext(), tenantID, draftID, req.ContentHTML); err != nil {
		return httpx.WriteError(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// revertToConstruction handles POST /v1/pecas/:id/voltar-para-construcao.
// Nulla sent_to_signing_at. Só permite quando ainda NÃO assinado — se já
// signed, devolve 404 (a UI trata como "não posso reverter" e refetch).
func (h *Handler) revertToConstruction(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	draftID := c.Params("id")
	if err := h.uc.RevertToConstruction(c.UserContext(), tenantID, draftID); err != nil {
		return httpx.WriteError(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

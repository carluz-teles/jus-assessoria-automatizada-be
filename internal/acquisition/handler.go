package acquisition

import (
	"context"
	"errors"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/httpx"
	"github.com/jusassessoria/platform/lib/httpx/middleware"
)

// errResumerNotWired is the cause of the typed 501 when no resumer port is wired.
var errResumerNotWired = errors.New("resumer port not wired")

// errLawyerLookupNotWired is the cause of the typed 501 when no lawyers port is wired.
var errLawyerLookupNotWired = errors.New("lawyer lookup port not wired")

// errAnalyzerNotWired is the cause of the typed 501 when no analyzer port is wired.
var errAnalyzerNotWired = errors.New("analyzer port not wired")

// roleAdmin is the product role allowed to activate integrations. It is the
// wire-level string the auth middleware puts on the principal (identity.Role
// widened to a string at the edge); activation is an onboarding action, so only
// ADMIN may call it — a LAWYER gets 403.
const roleAdmin = "ADMIN"

// handlerUC is the narrow port the Handler uses from the acquisition write use
// case — the integration activation/list methods.
type handlerUC interface {
	ActivateIntegration(ctx context.Context, tenantID string, scope Scope) (*Integration, error)
	ListIntegrations(ctx context.Context, tenantID string) ([]*Integration, error)
	// AssignResponsible sets/clears the responsável on the process behind a court_record
	// :id, in one tx. It returns only an error; the handler re-reads the ProcessoView
	// through the read port so the FE reidrates the header from the fresh row.
	AssignResponsible(ctx context.Context, tenantID, courtRecordID string, assignedUserID *string) error
	// UpdateProcessoManual grava a fase (override) e/ou o valor da causa preenchidos à mão
	// no cockpit. PATCH parcial (nil deixa o campo como está); o handler re-lê o ProcessoView.
	UpdateProcessoManual(ctx context.Context, tenantID, courtRecordID string, phaseOverride *string, claimValue *float64) error
	// BulkAssignResponsible atribui o responsável a vários processos (por ids ou toda a
	// faixa via All+filtros). Devolve a contagem afetada (court_record rows).
	BulkAssignResponsible(ctx context.Context, tenantID string, all bool, q ProcessosQuery, ids []string, assignedUserID *string) (int64, error)
	// Triagem da intimação: move the intimation's user_status to RESOLVED / IGNORED /
	// PENDING in one tx. Each returns only an error; the handler re-reads the detail view
	// through the read port so the FE reidrates the row from the fresh state.
	ResolveIntimacao(ctx context.Context, tenantID, intimationID string) error
	IgnoreIntimacao(ctx context.Context, tenantID, intimationID string) error
	ReopenIntimacao(ctx context.Context, tenantID, intimationID string) error
	// AssignIntimacaoAssignee sets/clears the single responsável de uma intimação em
	// uma tx. O handler re-lê o detail view depois pra reidratar o aside da FE.
	AssignIntimacaoAssignee(ctx context.Context, tenantID, intimationID string, assigneeUserID *string) error
	// BulkAssignIntimacoes atribui o responsável a várias intimações (por ids ou toda a
	// faixa via All+filtros). Devolve a contagem afetada.
	BulkAssignIntimacoes(ctx context.Context, tenantID string, all bool, q IntimacoesQuery, ids []string, assigneeUserID *string) (int64, error)
	// AddWatchedOAB adds one OAB to the tenant's DJEN watch (creating the integration
	// if it does not exist yet). ToggleWatchedOAB flips liga/desliga for an existing
	// watch — a 404 (ErrWatchedOABNotFound) if it was never added.
	AddWatchedOAB(ctx context.Context, tenantID string, oab OABEntry) (*WatchedOAB, error)
	ToggleWatchedOAB(ctx context.Context, tenantID, oab string, enabled bool) (*WatchedOAB, error)
}

// reader is the narrow port the Handler uses from the read use case — the
// keyset-paginated screen reads (each returns the page plus whether more remain).
type reader interface {
	Processos(ctx context.Context, q ProcessosQuery) (ProcessosResult, error)
	Processo(ctx context.Context, tenantID, id string) (ProcessoView, error)
	Intimacoes(ctx context.Context, q IntimacoesQuery) (IntimacoesResult, error)
	Intimacao(ctx context.Context, tenantID, id string) (IntimacaoDetailView, error)
	Andamentos(ctx context.Context, q AndamentosQuery) (AndamentosResult, error)
	ActivityLog(ctx context.Context, q ActivityLogQuery) (ActivityLogResult, error)
	IntimacoesByProcesso(ctx context.Context, q IntimacoesByProcessoQuery) (IntimacoesByProcessoResult, error)
	Partes(ctx context.Context, tenantID, courtRecordID string) (PartesView, error)
	ProcessosSummary(ctx context.Context, tenantID string) (ProcessosSummaryView, error)
	IntimacoesSummary(ctx context.Context, tenantID string) (IntimacoesSummaryView, error)
	ImportStatus(ctx context.Context, tenantID string) (ImportStatusView, error)
	Reconciliations(ctx context.Context, tenantID string) (ReconciliationsView, error)
	ReconciliationDetail(ctx context.Context, tenantID, jobID string) (ReconciliationDetailView, error)
	SyncRunItems(ctx context.Context, tenantID, syncRunID string) (SyncRunItemsView, error)
	Captures(ctx context.Context, tenantID string) (CapturesView, error)
	CaptureDetail(ctx context.Context, tenantID, id string) (CaptureRunView, error)
	WatchedOABs(ctx context.Context, tenantID string) ([]WatchedOABView, error)
}

// resumer is the optional AI-summary port the Handler exposes on GET
// /v1/processos/:id/resume. It is nil when the slice isn't wired for LLM summaries
// (same optional-port pattern as deadline's suggester).
type resumer interface {
	Resume(ctx context.Context, tenantID, courtRecordID string) (ProcessResumoView, error)
}

// analyzer is the optional AI-analysis port the Handler exposes on POST
// /v1/intimacoes/:id/analise ("Analisar esta intimação"). It is nil when the slice isn't
// wired for LLM analyses — a typed 501 (same optional-port pattern as resumer). The use
// case itself degrades internally when the LLM is off (returns an empty analysis), so a
// wired-but-unconfigured deploy still answers 200 with the pós-análise degraded state.
type analyzer interface {
	Analisar(ctx context.Context, tenantID, intimationID string) (IntimacaoAnaliseView, error)
}

// Handler is the acquisition HTTP surface. It owns its routing; the api only
// composes by calling RegisterV1.
type Handler struct {
	uc       handlerUC
	reader   reader
	resumer  resumer      // nil when no LLM summary port is wired
	analyzer analyzer     // nil when no LLM intimation-analysis port is wired
	lawyers  LawyerLookup // nil when the DJEN connector isn't wired (same optional-port pattern as resumer)
}

// NewHandler wires the handler to the acquisition write and read use cases.
// resumer, analyzer and lawyers are optional — nil disables their route surface with a
// typed 501 (/resume, /intimacoes/:id/analise and /oab-lookup). The aprovar/descartar
// buttons that used to live here moved to internal/actionitem's own confirmar/descartar
// endpoints (docs/erd-costura-providencia-tarefa-peca.md) — this slice no longer owns that
// lifecycle.
func NewHandler(uc handlerUC, reader reader, resumer resumer, analyzer analyzer, lawyers LawyerLookup) *Handler {
	return &Handler{uc: uc, reader: reader, resumer: resumer, analyzer: analyzer, lawyers: lawyers}
}

// RegisterV1 mounts acquisition's authenticated routes on the /v1 group. The
// write route is guarded by RequireRole(ADMIN); the reads are open to any
// authenticated principal of the tenant (scoped to its own rows).
func (h *Handler) RegisterV1(r fiber.Router) {
	r.Post("/acquisition/integrations", middleware.RequireRole(roleAdmin), h.activate)
	r.Get("/acquisition/integrations", h.list)
	r.Get("/acquisition/oab-lookup", h.oabLookup)
	r.Get("/acquisition/import-status", h.importStatus)
	r.Get("/acquisition/reconciliations", h.reconciliations)
	r.Get("/acquisition/reconciliations/:jobId", h.reconciliationDetail)
	r.Get("/acquisition/sync-runs/:syncRunId/items", h.syncRunItems)
	r.Get("/acquisition/captures", h.captures)
	r.Get("/acquisition/captures/:id", h.captureDetail)
	r.Get("/acquisition/watched-oabs", h.watchedOABs)
	r.Post("/acquisition/watched-oabs", middleware.RequireRole(roleAdmin), h.addWatchedOAB)
	r.Patch("/acquisition/watched-oabs/:oab", middleware.RequireRole(roleAdmin), h.toggleWatchedOAB)
	r.Get("/processos", h.listProcessos)
	// summary before :id so the static "/summary" segment is never shadowed by the param.
	r.Get("/processos/summary", h.processosSummary)
	r.Get("/processos/:id", h.getProcesso)
	r.Put("/processos/:id/responsavel", h.assignResponsible)
	r.Patch("/processos/:id", h.updateProcessoManual)
	r.Post("/processos/bulk/responsavel", h.bulkAssignResponsible)
	r.Get("/processos/:id/andamentos", h.listAndamentos)
	r.Get("/processos/:id/activity", h.listActivity)
	r.Get("/processos/:id/intimacoes", h.listIntimacoesByProcesso)
	r.Get("/processos/:id/partes", h.listPartes)
	r.Get("/processos/:id/resume", h.getResume)
	r.Get("/intimacoes", h.listIntimacoes)
	r.Get("/intimacoes/summary", h.intimacoesSummary)
	r.Get("/intimacoes/:id", h.getIntimacao)
	r.Post("/intimacoes/:id/resolve", h.resolveIntimacao)
	r.Post("/intimacoes/:id/ignore", h.ignoreIntimacao)
	r.Post("/intimacoes/:id/reopen", h.reopenIntimacao)
	r.Post("/intimacoes/:id/analise", h.analisarIntimacao)
	r.Put("/intimacoes/:id/responsavel", h.assignIntimacaoResponsavel)
	r.Post("/intimacoes/bulk/responsavel", h.bulkAssignIntimacaoResponsavel)
}

// Keyset sentinels for a first page (no cursor): the ascending processos scan
// starts below every row (” , zero uuid); the descending intimações scan starts
// above every row (max date, max uuid).
const (
	firstAscCNJ = ""
	zeroUUID    = "00000000-0000-0000-0000-000000000000"
	maxDate     = "9999-12-31"
	maxUUID     = "ffffffff-ffff-ffff-ffff-ffffffffffff"
	// maxTimestamp is the descending keyset's first-page sentinel for occurred_at (a
	// timestamptz, unlike intimações' date column) — above every real andamento.
	maxTimestamp = "9999-12-31T23:59:59Z"
)

// Query-param allowlists (docs/erd-backend.md §4e.3): each list route accepts only
// its own set — a client typo or a param belonging to another route is rejected with
// 400 instead of being silently ignored. Cursor pagination + filtering are the two
// documented mechanisms; body/other params never read query strings.
var (
	processosListParams  = paramSet("limit", "cursor", "search", "court", "lifecycle", "degree", "assignee")
	intimacoesListParams = paramSet("limit", "cursor", "search", "type", "user_status", "court", "urgencia", "work_stage", "nao_confirmado", "assignee")
)

// paramSet builds the route allowlist used by httpx.RejectUnknownParams.
func paramSet(params ...string) map[string]struct{} {
	set := make(map[string]struct{}, len(params))
	for _, p := range params {
		set[p] = struct{}{}
	}
	return set
}

// isKnownLifecycle accepts only the closed lifecycle set the filter exposes — the
// handler is the app-level CHECK on the ?lifecycle param (the DB stores text + the
// write path's own validation, but a read filter must not smuggle a bogus value into
// the closed-enum envelope).
func isKnownLifecycle(v string) bool {
	switch v {
	case LifecycleActive, LifecycleSuspended, LifecycleArchived, LifecycleSuperseded:
		return true
	default:
		return false
	}
}

// isKnownIntimacaoType accepts only the closed intimation-type set the filter exposes.
func isKnownIntimacaoType(v string) bool {
	switch v {
	case IntimationTypeIntimacao, IntimationTypeCitacao, IntimationTypeComunicacao:
		return true
	default:
		return false
	}
}

// isKnownIntimacaoUserStatus accepts only the closed intimation user-status set the
// filter exposes.
func isKnownIntimacaoUserStatus(v string) bool {
	switch v {
	case IntimationUserStatusPending, IntimationUserStatusResolved, IntimationUserStatusIgnored:
		return true
	default:
		return false
	}
}

// resolveIntimacaoAssignee turns the ?assignee filter into the value ListIntimacoes/
// CountIntimacoesFiltered filter on: "" is no filter, the sentinel "me" resolves to
// the principal's own id (the "Minhas" toggle), and any other value must be a
// well-formed uuid (a client error → 400 otherwise). Mirrors deadline's
// resolveAssignee (internal/deadline/handler.go:714) — kept as a local copy since
// slices only communicate by event, never by importing each other's code; the name
// is distinct from acquisition's own resolveAssignee (analise.go), which resolves an
// AI-suggested assignee against the firm's member list, a different concern.
func resolveIntimacaoAssignee(assignee, principalUserID string) (string, error) {
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

// isKnownUrgencia accepts only the closed urgência set the filter exposes — the
// handler is the app-level CHECK on the ?urgencia param. UrgenciaSemProvidencia is
// retained here as a DEPRECATED alias (redesign v1): it is no longer a real bucket,
// so the caller demotes it to "no filter" rather than rejecting — legacy deep-links
// degrade gracefully instead of 400-ing. UrgenciaNaoConfirmado is intentionally NOT
// here: it graduated to the separate ?nao_confirmado triage toggle.
func isKnownUrgencia(v string) bool {
	switch v {
	case UrgenciaAtraso, UrgenciaHoje, UrgenciaProximosDoisDias, UrgenciaSemana,
		UrgenciaEsteMes, UrgenciaMaisAdiante, UrgenciaSemDataDefinida,
		UrgenciaSemProvidencia:
		return true
	default:
		return false
	}
}

// isKnownWorkStage valida o ?work_stage contra o conjunto fechado dos WorkStage*
// (o filtro de Status da inbox). Vazio = sem filtro (tratado antes de chamar).
func isKnownWorkStage(v string) bool {
	switch v {
	case WorkStageReceived, WorkStageAwaitingConfirmation, WorkStageConfirmed,
		WorkStageDrafting, WorkStagePartnerReview, WorkStageFiled:
		return true
	default:
		return false
	}
}

// integrationView is the read model returned to the client — a per-endpoint DTO.
// credential_ref is deliberately absent: it is a server-side secret pointer.
type integrationView struct {
	ID        string    `json:"id"`
	Source    string    `json:"source"`
	Scope     Scope     `json:"scope"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// listEnvelope is the {data:[...]} response shape shared by both endpoints. The
// integration set per tenant is tiny (one row per source), so it is returned
// whole — no cursor pagination.
type listEnvelope struct {
	Data []integrationView `json:"data"`
}

// activate handles POST /v1/acquisition/integrations: validates the body,
// activates the tenant's DJEN watch under the given scope (row + event in one
// tx), and returns 201 with the activated integration. tenant_id comes from the
// verified principal, never the body.
func (h *Handler) activate(c *fiber.Ctx) error {
	var req ActivateIntegrationRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteError(c, apperr.NewInvalid("malformed request body"))
	}
	if err := req.Validate(); err != nil {
		return httpx.WriteValidationError(c, err)
	}

	tenantID := httpx.TenantFromCtx(c)
	integration, err := h.uc.ActivateIntegration(c.UserContext(), tenantID, req.Scope)
	if err != nil {
		return httpx.WriteError(c, err)
	}

	return c.Status(fiber.StatusCreated).JSON(newListEnvelope([]*Integration{integration}))
}

// list handles GET /v1/acquisition/integrations: returns the tenant's
// integrations. tenant_id comes from the principal, so a caller only ever sees
// its own escritório's rows.
func (h *Handler) list(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	integrations, err := h.uc.ListIntegrations(c.UserContext(), tenantID)
	if err != nil {
		return httpx.WriteError(c, err)
	}

	return c.Status(fiber.StatusOK).JSON(newListEnvelope(integrations))
}

// lawyerView is the response shape of GET /v1/acquisition/oab-lookup.
type lawyerView struct {
	OAB  string `json:"oab"`
	Name string `json:"name"`
}

// oabLookup handles GET /v1/acquisition/oab-lookup?oab=SP123456: a best-effort
// name lookup that reuses the DJEN connector (see oab_lookup.go for why — no
// free public OAB registry API exists). Any authenticated user may call it (no
// tenant needed — it fires during onboarding, before the tenant may even be
// fully set up). ErrOABNotFound is a normal, expected 404 — the caller (the
// wizard) degrades to a placeholder, so this is never a hard failure.
func (h *Handler) oabLookup(c *fiber.Ctx) error {
	if h.lawyers == nil {
		return httpx.WriteError(c, apperr.NewUnavailable("oab lookup indisponível: provisor não configurado", errLawyerLookupNotWired))
	}

	oab, err := parseOAB(c.Query("oab"))
	if err != nil {
		return httpx.WriteError(c, err)
	}

	name, err := h.lawyers.LookupOABName(c.UserContext(), oab)
	if err != nil {
		return httpx.WriteError(c, err)
	}

	return c.Status(fiber.StatusOK).JSON(lawyerView{OAB: c.Query("oab"), Name: name})
}

// importStatus handles GET /v1/acquisition/import-status: the tenant's onboarding
// backfill state ({importing, status, total/ok/error slices}) for the FE banner
// "importando seus processos…". tenant_id comes from the principal; a tenant with no
// backfill_job gets status NONE (banner hidden).
func (h *Handler) importStatus(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	status, err := h.reader.ImportStatus(c.UserContext(), tenantID)
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(status)
}

// reconciliations serves the reconciliations screen: import state + acquired
// totals + recent executions, straight from the read model (no envelope — the
// view is already the whole payload).
func (h *Handler) reconciliations(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	view, err := h.reader.Reconciliations(c.UserContext(), tenantID)
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(view)
}

// reconciliationDetail handles GET /v1/acquisition/reconciliations/:jobId: one
// import's reconciliation header + its per-window (sync_run) table. A miss is 404.
func (h *Handler) reconciliationDetail(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	view, err := h.reader.ReconciliationDetail(c.UserContext(), tenantID, c.Params("jobId"))
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(view)
}

// captures serves the "Capturas" screen: the KPI header + one row per capture (daily
// fan-out, enrichment day, initial load), straight from the read model (no envelope —
// the view is the whole payload, bounded, no pagination in v0).
func (h *Handler) captures(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	view, err := h.reader.Captures(c.UserContext(), tenantID)
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(view)
}

// captureDetail handles GET /v1/acquisition/captures/:id: one capture's detail (a
// DAILY_CAPTURE or ENRICHMENT capture_run). A miss is the typed 404. The initial load
// keeps its existing reconciliation endpoint.
func (h *Handler) captureDetail(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	view, err := h.reader.CaptureDetail(c.UserContext(), tenantID, c.Params("id"))
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(view)
}

// watchedOABView is the per-item response shape of GET /v1/acquisition/watched-oabs,
// and also what POST/PATCH answer with (Name omitted — those two never know it, no
// capture may have happened yet). Enabled backs the Termos liga/desliga toggle.
type watchedOABView struct {
	OAB          string     `json:"oab"`
	Name         *string    `json:"name,omitempty"`
	Enabled      bool       `json:"enabled"`
	LastAction   *string    `json:"last_action,omitempty"`
	LastActionAt *time.Time `json:"last_action_at,omitempty"`
}

// watchedOABsEnvelope wraps the list in {data:[...]}.
type watchedOABsEnvelope struct {
	Data []watchedOABView `json:"data"`
}

// watchedOABs handles GET /v1/acquisition/watched-oabs: the tenant's monitored OABs
// (DJEN integration) with the most-frequent lawyer name from party_counsel. Name is
// null when no capture has occurred yet for that OAB. Used by the Termos settings
// tab to render the card header as "NOME — OAB" or just "OAB" when name is absent.
func (h *Handler) watchedOABs(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	views, err := h.reader.WatchedOABs(c.UserContext(), tenantID)
	if err != nil {
		return httpx.WriteError(c, err)
	}
	items := make([]watchedOABView, 0, len(views))
	for _, v := range views {
		items = append(items, watchedOABView{
			OAB: v.OAB, Name: v.Name, Enabled: v.Enabled,
			LastAction: v.LastAction, LastActionAt: v.LastActionAt,
		})
	}
	return c.Status(fiber.StatusOK).JSON(watchedOABsEnvelope{Data: items})
}

// addWatchedOAB handles POST /v1/acquisition/watched-oabs: adds one OAB to the
// tenant's DJEN watch (creating the integration if this is the tenant's first OAB).
// tenant_id comes from the principal, never the body.
func (h *Handler) addWatchedOAB(c *fiber.Ctx) error {
	var req AddWatchedOABRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteError(c, apperr.NewInvalid("malformed request body"))
	}
	if err := req.Validate(); err != nil {
		return httpx.WriteValidationError(c, err)
	}

	oab, err := parseOAB(req.OAB)
	if err != nil {
		return httpx.WriteError(c, err)
	}

	tenantID := httpx.TenantFromCtx(c)
	added, err := h.uc.AddWatchedOAB(c.UserContext(), tenantID, oab)
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(watchedOABView{OAB: added.OAB, Enabled: added.Enabled})
}

// toggleWatchedOAB handles PATCH /v1/acquisition/watched-oabs/:oab: the Termos
// liga/desliga switch — {enabled:false} pauses future capture for this OAB while
// keeping everything already captured visible; {enabled:true} resumes it and, if it
// was actually disabled, triggers a catch-up sync scoped to the downtime. A miss
// (never added) is the typed 404. tenant_id comes from the principal.
func (h *Handler) toggleWatchedOAB(c *fiber.Ctx) error {
	oab := c.Params("oab")
	if !oabRegex.MatchString(oab) {
		return httpx.WriteError(c, apperr.NewInvalid("oab must be UF (2 letters) + 1-6 digits, e.g. SP123456"))
	}

	var req ToggleWatchedOABRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteError(c, apperr.NewInvalid("malformed request body"))
	}

	tenantID := httpx.TenantFromCtx(c)
	updated, err := h.uc.ToggleWatchedOAB(c.UserContext(), tenantID, oab, req.Enabled)
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(watchedOABView{OAB: updated.OAB, Enabled: updated.Enabled})
}

// syncRunItems handles GET /v1/acquisition/sync-runs/:syncRunId/items: the collapse
// payload for one window — the processes and intimations it first discovered.
func (h *Handler) syncRunItems(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	view, err := h.reader.SyncRunItems(c.UserContext(), tenantID, c.Params("syncRunId"))
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(view)
}

// listProcessos handles GET /v1/processos: the tenant's live processes, keyset
// paginated (?limit, ?cursor) and filterable by ?search (cnj_number ILIKE), ?court /
// ?degree (free text from the envelope's options), ?lifecycle (a closed set) and
// ?assignee (a user id). A param outside the route's allowlist is a client error →
// 400; an unknown lifecycle or malformed assignee is likewise 400. tenant_id comes
// from the principal.
func (h *Handler) listProcessos(c *fiber.Ctx) error {
	if err := httpx.RejectUnknownParams(c, processosListParams); err != nil {
		return httpx.WriteError(c, err)
	}
	tenantID := httpx.TenantFromCtx(c)
	limit := httpx.ClampLimit(c.QueryInt("limit"), httpx.DefaultLimit, httpx.MaxLimit)

	lifecycle := c.Query("lifecycle")
	if lifecycle != "" && !isKnownLifecycle(lifecycle) {
		return httpx.WriteError(c, apperr.NewInvalid("invalid lifecycle filter"))
	}
	assignee := c.Query("assignee")
	if assignee != "" {
		if _, err := uuid.Parse(assignee); err != nil {
			return httpx.WriteError(c, apperr.NewInvalid("invalid assignee filter (want a user id)"))
		}
	}

	lastCNJ, lastID := firstAscCNJ, zeroUUID
	if tok := c.Query("cursor"); tok != "" {
		cur, err := httpx.DecodeCursor(tok)
		if err != nil {
			return httpx.WriteError(c, err)
		}
		lastCNJ, lastID = cur.LastSortValue, cur.LastID
	}

	res, err := h.reader.Processos(c.UserContext(), ProcessosQuery{
		TenantID: tenantID, LastCNJ: lastCNJ, LastID: lastID, Limit: limit,
		Search:    c.Query("search"),
		Court:     c.Query("court"),
		Lifecycle: lifecycle,
		Degree:    c.Query("degree"),
		Assignee:  assignee,
	})
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(newProcessosPage(res, limit))
}

// listIntimacoes handles GET /v1/intimacoes: the tenant's intimation inbox, newest
// availability first, keyset paginated (?limit, ?cursor) and filterable by ?search
// (the court record's cnj_number ILIKE), ?type / ?user_status (closed sets) and
// ?court (free text from the envelope's options). Unknown params/lifecycle values are
// client errors → 400.
func (h *Handler) listIntimacoes(c *fiber.Ctx) error {
	if err := httpx.RejectUnknownParams(c, intimacoesListParams); err != nil {
		return httpx.WriteError(c, err)
	}
	tenantID := httpx.TenantFromCtx(c)
	limit := httpx.ClampLimit(c.QueryInt("limit"), httpx.DefaultLimit, httpx.MaxLimit)

	typ := c.Query("type")
	if typ != "" && !isKnownIntimacaoType(typ) {
		return httpx.WriteError(c, apperr.NewInvalid("invalid type filter"))
	}
	userStatus := c.Query("user_status")
	if userStatus != "" && !isKnownIntimacaoUserStatus(userStatus) {
		return httpx.WriteError(c, apperr.NewInvalid("invalid user_status filter"))
	}
	urgencia := c.Query("urgencia")
	if urgencia != "" && !isKnownUrgencia(urgencia) {
		return httpx.WriteError(c, apperr.NewInvalid("invalid urgencia filter"))
	}
	// Deprecated alias: demote "sem_providencia" to "no filter" so legacy deep-links
	// keep working instead of erroring out.
	if urgencia == UrgenciaSemProvidencia {
		urgencia = ""
	}
	workStage := c.Query("work_stage")
	if workStage != "" && !isKnownWorkStage(workStage) {
		return httpx.WriteError(c, apperr.NewInvalid("invalid work_stage filter"))
	}
	naoConfirmado := c.Query("nao_confirmado") == "true" || c.Query("nao_confirmado") == "1"
	p, ok := httpx.PrincipalFromCtx(c)
	if !ok {
		return httpx.WriteError(c, apperr.NewUnauthorized("missing principal"))
	}
	assignee, err := resolveIntimacaoAssignee(c.Query("assignee"), p.UserID)
	if err != nil {
		return httpx.WriteError(c, err)
	}

	lastMade, lastID := maxDate, maxUUID
	if tok := c.Query("cursor"); tok != "" {
		cur, err := httpx.DecodeCursor(tok)
		if err != nil {
			return httpx.WriteError(c, err)
		}
		lastMade, lastID = cur.LastSortValue, cur.LastID
	}

	res, err := h.reader.Intimacoes(c.UserContext(), IntimacoesQuery{
		TenantID: tenantID, LastMadeAvailable: lastMade, LastID: lastID, Limit: limit,
		Search:        c.Query("search"),
		Type:          typ,
		UserStatus:    userStatus,
		Court:         c.Query("court"),
		Urgencia:      urgencia,
		WorkStage:     workStage,
		NaoConfirmado: naoConfirmado,
		Assignee:      assignee,
	})
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(newIntimacoesPage(res, limit))
}

// getIntimacao handles GET /v1/intimacoes/:id: one intimation for the FE deep-link (open
// the detail of an intimation not on the loaded inbox page). The view is the whole
// payload — one IntimacaoView, same shape as a list row — so it is returned without a
// list envelope. tenant_id comes from the principal, never the path/body: a miss or a
// foreign tenant's id is the read model's typed 404, a non-uuid id its typed 400.
func (h *Handler) getIntimacao(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	view, err := h.reader.Intimacao(c.UserContext(), tenantID, c.Params("id"))
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(view)
}

// resolveIntimacao / ignoreIntimacao / reopenIntimacao handle the POST triagem actions on
// GET /v1/intimacoes/:id/{resolve,ignore,reopen}: move the intimation's user_status and
// answer 200 with the fresh detail view (re-read through the read port), so the FE
// reidrates the row from the persisted state. tenant_id comes from the principal, never
// the path/body; a miss or a foreign tenant's id is the write's typed 404, a non-uuid id
// its typed 400. No body — the verb is the whole intent.
func (h *Handler) resolveIntimacao(c *fiber.Ctx) error {
	return h.triageIntimacao(c, h.uc.ResolveIntimacao)
}

func (h *Handler) ignoreIntimacao(c *fiber.Ctx) error {
	return h.triageIntimacao(c, h.uc.IgnoreIntimacao)
}

func (h *Handler) reopenIntimacao(c *fiber.Ctx) error {
	return h.triageIntimacao(c, h.uc.ReopenIntimacao)
}

// triageIntimacao is the shared body of the three triagem handlers: run the action in one
// tx, then re-read and return the detail view. The action func is the use case's
// Resolve/Ignore/Reopen — same signature — so the three handlers differ only by the verb.
func (h *Handler) triageIntimacao(c *fiber.Ctx, action func(ctx context.Context, tenantID, intimationID string) error) error {
	tenantID := httpx.TenantFromCtx(c)
	id := c.Params("id")
	if err := action(c.UserContext(), tenantID, id); err != nil {
		return httpx.WriteError(c, err)
	}
	view, err := h.reader.Intimacao(c.UserContext(), tenantID, id)
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(view)
}

// assignIntimacaoResponsavel handles PUT /v1/intimacoes/:id/responsavel: set (or clear,
// with null) o responsável único da intimação (0057, ex-conductor/reviewer). O write
// roda em uma tx (guard de membership, então write); depois o handler re-lê o detail view
// pra que a FE reidrate o aside a partir da linha fresca. tenant_id vem do principal,
// nunca do body.
func (h *Handler) assignIntimacaoResponsavel(c *fiber.Ctx) error {
	var req AssignIntimacaoResponsavelRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteError(c, apperr.NewInvalid("malformed request body"))
	}
	if err := req.Validate(); err != nil {
		return httpx.WriteValidationError(c, err)
	}

	tenantID := httpx.TenantFromCtx(c)
	id := c.Params("id")
	if err := h.uc.AssignIntimacaoAssignee(c.UserContext(), tenantID, id, req.AssigneeUserID); err != nil {
		return httpx.WriteError(c, err)
	}

	view, err := h.reader.Intimacao(c.UserContext(), tenantID, id)
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(view)
}

// bulkAssignIntimacaoResponsavel handles POST /v1/intimacoes/bulk/responsavel: atribui
// o responsável a várias intimações. All=true aplica a TODA a faixa/filtro atual (inclui
// os não paginados) reusando os filtros do GET /intimacoes; senão aplica aos ids.
// tenant_id vem do principal, nunca do body.
func (h *Handler) bulkAssignIntimacaoResponsavel(c *fiber.Ctx) error {
	var req BulkAssignResponsavelRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteError(c, apperr.NewInvalid("malformed request body"))
	}
	if err := req.Validate(); err != nil {
		return httpx.WriteValidationError(c, err)
	}
	if req.All && req.Urgencia != "" && !isKnownUrgencia(req.Urgencia) {
		return httpx.WriteError(c, apperr.NewInvalid("invalid urgencia filter"))
	}
	// Deprecated alias: demote "sem_providencia" to "no filter" for legacy deep-links.
	if req.Urgencia == UrgenciaSemProvidencia {
		req.Urgencia = ""
	}

	tenantID := httpx.TenantFromCtx(c)
	p, ok := httpx.PrincipalFromCtx(c)
	if !ok {
		return httpx.WriteError(c, apperr.NewUnauthorized("missing principal"))
	}
	assignee, err := resolveIntimacaoAssignee(req.Assignee, p.UserID)
	if err != nil {
		return httpx.WriteError(c, err)
	}

	q := IntimacoesQuery{
		TenantID:      tenantID,
		Search:        req.Search,
		Type:          req.Type,
		UserStatus:    req.UserStatus,
		Court:         req.Court,
		Urgencia:      req.Urgencia,
		NaoConfirmado: req.NaoConfirmado,
		Assignee:      assignee,
	}
	n, err := h.uc.BulkAssignIntimacoes(c.UserContext(), tenantID, req.All, q, req.IDs, req.AssigneeUserID)
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(fiber.Map{"affected": n})
}

// processosSummary handles GET /v1/processos/summary: the processes list KPI counts
// (bucketed by court_record lifecycle) for the tenant. A single read model (no cursor
// envelope). tenant_id comes from the principal.
func (h *Handler) processosSummary(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	view, err := h.reader.ProcessosSummary(c.UserContext(), tenantID)
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(view)
}

// intimacoesSummary handles GET /v1/intimacoes/summary: the intimações inbox KPI counts
// (bucketed by triagem state) for the tenant. A single read model (no cursor envelope).
// tenant_id comes from the principal.
func (h *Handler) intimacoesSummary(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	view, err := h.reader.IntimacoesSummary(c.UserContext(), tenantID)
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(view)
}

// getProcesso handles GET /v1/processos/:id: one process for the FE deep-link (open the
// detail of a process not on the loaded list page). The view is the whole payload — one
// ProcessoView, same shape as a list row — so it is returned without a list envelope.
// tenant_id comes from the principal, never the path/body: a miss or a foreign tenant's
// id is the read model's typed 404, a non-uuid id its typed 400.
func (h *Handler) getProcesso(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	view, err := h.reader.Processo(c.UserContext(), tenantID, c.Params("id"))
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(view)
}

// getResume handles GET /v1/processos/:id/resume: the AI-generated process summary
// (sync-on-first-GET, write-once, serve-from-cache thereafter — the ResumoUseCase
// owns that policy). tenant_id comes from the principal, never the path/body: a miss
// or a foreign tenant's id is the read's typed 404, a non-uuid id its typed 400. A nil
// resumer (slice not wired for LLM summaries) is a typed 501 — the route exists but no
// provider is configured, which the api composes away by always wiring the use case.
func (h *Handler) getResume(c *fiber.Ctx) error {
	if h.resumer == nil {
		return httpx.WriteError(c, apperr.NewUnavailable("resumo de processo indisponível: provisor não configurado", errResumerNotWired))
	}
	tenantID := httpx.TenantFromCtx(c)
	view, err := h.resumer.Resume(c.UserContext(), tenantID, c.Params("id"))
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(view)
}

// analisarIntimacao handles POST /v1/intimacoes/:id/analise ("Analisar esta intimação"):
// the IA reads the teor and produces "O que aconteceu" (summary) + "Providências derivadas"
// (providencias), persists them (OVERWRITE — re-executable), and returns the analysis. The
// use case degrades internally (LLM off/erro → empty analysis, still persisted+returned), so
// this handler only surfaces the read's typed 404/400 (unknown/foreign/non-uuid id). A nil
// analyzer (slice not wired for LLM) is a typed 501 — the route exists but no provider is
// configured, which the api composes away by always wiring the use case. tenant_id comes
// from the principal, never the path/body.
func (h *Handler) analisarIntimacao(c *fiber.Ctx) error {
	if h.analyzer == nil {
		return httpx.WriteError(c, apperr.NewUnavailable("análise de intimação indisponível: provisor não configurado", errAnalyzerNotWired))
	}
	tenantID := httpx.TenantFromCtx(c)
	view, err := h.analyzer.Analisar(c.UserContext(), tenantID, c.Params("id"))
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(view)
}

// assignResponsible handles PUT /v1/processos/:id/responsavel: set (or clear, with a null
// user_id) the responsável for the process behind the court_record :id. The write runs in
// one tx (resolve record→case, guard the user is a member, assign); then the handler
// re-reads the ProcessoView through the read port and answers 200 with it, so the FE
// reidrates the header from the fresh row rather than trusting a client echo. tenant_id
// comes from the principal, never the body — a foreign/unknown :id is the write's typed
// 404, a malformed body (non-uuid user_id) its typed 400.
func (h *Handler) assignResponsible(c *fiber.Ctx) error {
	var req AssignResponsibleRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteError(c, apperr.NewInvalid("malformed request body"))
	}
	if err := req.Validate(); err != nil {
		return httpx.WriteValidationError(c, err)
	}

	tenantID := httpx.TenantFromCtx(c)
	id := c.Params("id")
	if err := h.uc.AssignResponsible(c.UserContext(), tenantID, id, req.UserID); err != nil {
		return httpx.WriteError(c, err)
	}

	view, err := h.reader.Processo(c.UserContext(), tenantID, id)
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(view)
}

// updateProcessoManual handles PATCH /v1/processos/:id: grava a fase (override manual) e/ou o
// valor da causa preenchidos à mão no cockpit. PATCH parcial — só os campos presentes no body
// são escritos. Re-lê o ProcessoView e responde 200 com ele, pra o FE reidratar o header da
// linha fresca. tenant_id vem do principal; um :id inválido/foreign é o 404/400 tipado do write.
func (h *Handler) updateProcessoManual(c *fiber.Ctx) error {
	var req UpdateProcessoManualRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteError(c, apperr.NewInvalid("malformed request body"))
	}
	if err := req.Validate(); err != nil {
		return httpx.WriteValidationError(c, err)
	}

	tenantID := httpx.TenantFromCtx(c)
	id := c.Params("id")
	if err := h.uc.UpdateProcessoManual(c.UserContext(), tenantID, id, req.Phase, req.ClaimValue); err != nil {
		return httpx.WriteError(c, err)
	}

	view, err := h.reader.Processo(c.UserContext(), tenantID, id)
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(view)
}

// bulkAssignResponsible handles POST /v1/processos/bulk/responsavel: atribui o
// responsável a vários processos de uma vez. All=true aplica a TODA a faixa/filtro
// atual (inclui os não paginados) reusando os filtros do GET /processos; senão
// aplica aos ids (court_record ids — mesma granularidade do PUT
// /processos/:id/responsavel). Mesmo padrão do bulk de intimações
// (bulkAssignIntimacaoResponsavel). tenant_id vem do principal, nunca do body.
func (h *Handler) bulkAssignResponsible(c *fiber.Ctx) error {
	var req BulkAssignResponsibleRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteError(c, apperr.NewInvalid("malformed request body"))
	}
	if err := req.Validate(); err != nil {
		return httpx.WriteValidationError(c, err)
	}
	if req.All && req.Lifecycle != "" && !isKnownLifecycle(req.Lifecycle) {
		return httpx.WriteError(c, apperr.NewInvalid("invalid lifecycle filter"))
	}
	if req.Assignee != "" {
		if _, err := uuid.Parse(req.Assignee); err != nil {
			return httpx.WriteError(c, apperr.NewInvalid("invalid assignee filter (want a user id)"))
		}
	}

	tenantID := httpx.TenantFromCtx(c)
	q := ProcessosQuery{
		TenantID:  tenantID,
		Search:    req.Search,
		Court:     req.Court,
		Lifecycle: req.Lifecycle,
		Degree:    req.Degree,
		Assignee:  req.Assignee,
	}
	n, err := h.uc.BulkAssignResponsible(c.UserContext(), tenantID, req.All, q, req.IDs, req.UserID)
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(fiber.Map{"affected": n})
}

// listAndamentos handles GET /v1/processos/:id/andamentos: the "Andamentos" tab of one
// process — its docket entries, newest first, keyset paginated (?limit, ?cursor). The
// :id is the court_record id (the same id /processos returns); tenant_id comes from the
// principal, and the read is tenant-scoped so a foreign :id yields an empty page.
func (h *Handler) listAndamentos(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	limit := httpx.ClampLimit(c.QueryInt("limit"), httpx.DefaultLimit, httpx.MaxLimit)

	lastOccurred, lastID := maxTimestamp, maxUUID
	if tok := c.Query("cursor"); tok != "" {
		cur, err := httpx.DecodeCursor(tok)
		if err != nil {
			return httpx.WriteError(c, err)
		}
		lastOccurred, lastID = cur.LastSortValue, cur.LastID
	}

	res, err := h.reader.Andamentos(c.UserContext(), AndamentosQuery{
		TenantID: tenantID, CourtRecordID: c.Params("id"),
		LastOccurred: lastOccurred, LastID: lastID, Limit: limit,
	})
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(newAndamentosPage(res, limit))
}

// listActivity handles GET /v1/processos/:id/activity: the "Atividade" tab of one
// process — its activity log (migration 0073), newest first, keyset paginated
// (?limit, ?cursor). The :id is the court_record id (the same id /processos
// returns); tenant_id comes from the principal, and the read is tenant-scoped so a
// foreign :id yields an empty page.
func (h *Handler) listActivity(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	limit := httpx.ClampLimit(c.QueryInt("limit"), httpx.DefaultLimit, httpx.MaxLimit)

	lastOccurred, lastID := maxTimestamp, maxUUID
	if tok := c.Query("cursor"); tok != "" {
		cur, err := httpx.DecodeCursor(tok)
		if err != nil {
			return httpx.WriteError(c, err)
		}
		lastOccurred, lastID = cur.LastSortValue, cur.LastID
	}

	res, err := h.reader.ActivityLog(c.UserContext(), ActivityLogQuery{
		TenantID: tenantID, CourtRecordID: c.Params("id"),
		LastOccurred: lastOccurred, LastID: lastID, Limit: limit,
	})
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(newActivityLogPage(res, limit))
}

// listIntimacoesByProcesso handles GET /v1/processos/:id/intimacoes: the "Intimações" tab
// of one process — its intimations, newest availability first, keyset paginated (?limit,
// ?cursor). The :id is the court_record id (the same id /processos returns); tenant_id
// comes from the principal, and the read is tenant-scoped so a foreign :id yields an
// empty page.
func (h *Handler) listIntimacoesByProcesso(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	limit := httpx.ClampLimit(c.QueryInt("limit"), httpx.DefaultLimit, httpx.MaxLimit)

	lastMade, lastID := maxDate, maxUUID
	if tok := c.Query("cursor"); tok != "" {
		cur, err := httpx.DecodeCursor(tok)
		if err != nil {
			return httpx.WriteError(c, err)
		}
		lastMade, lastID = cur.LastSortValue, cur.LastID
	}

	res, err := h.reader.IntimacoesByProcesso(c.UserContext(), IntimacoesByProcessoQuery{
		TenantID: tenantID, CourtRecordID: c.Params("id"),
		LastMadeAvailable: lastMade, LastID: lastID, Limit: limit,
	})
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(newIntimacoesByProcessoPage(res, limit))
}

// listPartes handles GET /v1/processos/:id/partes: the "Partes" cards of one process —
// its autor/réu/terceiros with each party's advogados, bucketed by role. The :id is the
// court_record id (the same id /processos returns); tenant_id comes from the principal,
// and the read is tenant-scoped so a foreign :id yields three empty buckets. The view is
// the whole payload (no cursor — the party set per process is tiny), so it is returned
// without an envelope. A non-uuid :id is the read model's typed 400.
func (h *Handler) listPartes(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	view, err := h.reader.Partes(c.UserContext(), tenantID, c.Params("id"))
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(view)
}

// newProcessosPage wraps the processos read model in the cursor envelope; the next
// cursor keys off the last row's (cnj_number, id) and the totals carry "X de Y".
func newProcessosPage(res ProcessosResult, limit int) httpx.Page[ProcessoView] {
	items := res.Items
	if items == nil {
		items = []ProcessoView{}
	}
	meta := httpx.PageMeta{Limit: limit, TotalCount: res.TotalCount, Total: res.Total}
	if res.HasMore && len(items) > 0 {
		last := items[len(items)-1]
		tok := httpx.EncodeCursor(httpx.Cursor{LastID: last.ID, LastSortValue: last.CNJNumber})
		meta.NextCursor = &tok
	}
	return httpx.Page[ProcessoView]{Data: items, Page: meta, Filters: res.Filters.NonNil()}
}

// intimacoesEnvelope is the list response for GET /v1/intimacoes. It extends the
// standard cursor envelope with a `buckets` object: per-urgência counts computed
// over the same non-urgência filters so section headers (Em atraso / Vence hoje /
// Esta semana / Mais adiante / Sem prazo) agree with the list without an N+1 call.
type intimacoesEnvelope struct {
	httpx.Page[IntimacaoView]
	Buckets IntimacaoBucketsView `json:"buckets"`
}

// newIntimacoesPage wraps the intimações read model in the cursor envelope; the
// next cursor keys off the last row's (made_available_at, id) and the totals "X de Y".
func newIntimacoesPage(res IntimacoesResult, limit int) intimacoesEnvelope {
	items := res.Items
	if items == nil {
		items = []IntimacaoView{}
	}
	meta := httpx.PageMeta{Limit: limit, TotalCount: res.TotalCount, Total: res.Total}
	if res.HasMore && len(items) > 0 {
		last := items[len(items)-1]
		tok := httpx.EncodeCursor(httpx.Cursor{
			LastID:        last.ID,
			LastSortValue: last.MadeAvailableAt.Format(time.DateOnly),
		})
		meta.NextCursor = &tok
	}
	return intimacoesEnvelope{
		Page:    httpx.Page[IntimacaoView]{Data: items, Page: meta, Filters: res.Filters.NonNil()},
		Buckets: res.Buckets,
	}
}

// newAndamentosPage wraps the andamentos read model in the cursor envelope; the next
// cursor keys off the last row's (occurred_at, id). There is no search on this tab, so
// the "X de Y" totals coincide (both the process's andamento count).
func newAndamentosPage(res AndamentosResult, limit int) httpx.Page[AndamentoView] {
	items := res.Items
	if items == nil {
		items = []AndamentoView{}
	}
	meta := httpx.PageMeta{Limit: limit, TotalCount: res.Total, Total: res.Total}
	if res.HasMore && len(items) > 0 {
		last := items[len(items)-1]
		tok := httpx.EncodeCursor(httpx.Cursor{
			LastID:        last.ID,
			LastSortValue: last.OccurredAt.Format(time.RFC3339Nano),
		})
		meta.NextCursor = &tok
	}
	return httpx.Page[AndamentoView]{Data: items, Page: meta, Filters: res.Filters.NonNil()}
}

// newIntimacoesByProcessoPage wraps the per-process intimations read model in the cursor
// envelope; the next cursor keys off the last row's (made_available_at, id). There is no
// search on this tab, so the "X de Y" totals coincide (both the process's intimation count).
// newActivityLogPage assembles the Atividade tab's paginated envelope, mirroring
// newAndamentosPage: the cursor sorts on OccurredAt (descending keyset).
func newActivityLogPage(res ActivityLogResult, limit int) httpx.Page[ActivityLogView] {
	items := res.Items
	if items == nil {
		items = []ActivityLogView{}
	}
	meta := httpx.PageMeta{Limit: limit, TotalCount: res.Total, Total: res.Total}
	if res.HasMore && len(items) > 0 {
		last := items[len(items)-1]
		tok := httpx.EncodeCursor(httpx.Cursor{
			LastID:        last.ID,
			LastSortValue: last.OccurredAt.Format(time.RFC3339Nano),
		})
		meta.NextCursor = &tok
	}
	return httpx.Page[ActivityLogView]{Data: items, Page: meta, Filters: res.Filters.NonNil()}
}

func newIntimacoesByProcessoPage(res IntimacoesByProcessoResult, limit int) httpx.Page[IntimacaoView] {
	items := res.Items
	if items == nil {
		items = []IntimacaoView{}
	}
	meta := httpx.PageMeta{Limit: limit, TotalCount: res.Total, Total: res.Total}
	if res.HasMore && len(items) > 0 {
		last := items[len(items)-1]
		tok := httpx.EncodeCursor(httpx.Cursor{
			LastID:        last.ID,
			LastSortValue: last.MadeAvailableAt.Format(time.DateOnly),
		})
		meta.NextCursor = &tok
	}
	return httpx.Page[IntimacaoView]{Data: items, Page: meta, Filters: res.Filters.NonNil()}
}

// newListEnvelope maps entities to the client-facing envelope. The data slice is
// always initialized so an empty result serializes as [] rather than null.
func newListEnvelope(integrations []*Integration) listEnvelope {
	views := make([]integrationView, 0, len(integrations))
	for _, integ := range integrations {
		views = append(views, integrationView{
			ID:        integ.ID,
			Source:    integ.Source,
			Scope:     integ.Scope,
			Status:    integ.Status,
			CreatedAt: integ.CreatedAt,
			UpdatedAt: integ.UpdatedAt,
		})
	}
	return listEnvelope{Data: views}
}

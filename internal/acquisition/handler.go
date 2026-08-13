package acquisition

import (
	"context"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/httpx"
	"github.com/jusassessoria/platform/lib/httpx/middleware"
)

// roleAdmin is the product role allowed to activate integrations. It is the
// wire-level string the auth middleware puts on the principal (identity.Role
// widened to a string at the edge); activation is an onboarding action, so only
// ADMIN may call it — a LAWYER gets 403.
const roleAdmin = "ADMIN"

// handlerUC is the narrow port the Handler uses from the acquisition write use
// case — the integration activation/list methods.
type handlerUC interface {
	ActivateIntegration(ctx context.Context, tenantID string, sources []string, scope Scope) ([]*Integration, error)
	ListIntegrations(ctx context.Context, tenantID string) ([]*Integration, error)
	// AssignResponsible sets/clears the responsável on the process behind a court_record
	// :id, in one tx. It returns only an error; the handler re-reads the ProcessoView
	// through the read port so the FE reidrates the header from the fresh row.
	AssignResponsible(ctx context.Context, tenantID, courtRecordID string, assignedUserID *string) error
	// Triagem da intimação: move the intimation's user_status to RESOLVED / IGNORED /
	// PENDING in one tx. Each returns only an error; the handler re-reads the detail view
	// through the read port so the FE reidrates the row from the fresh state.
	ResolveIntimacao(ctx context.Context, tenantID, intimationID string) error
	IgnoreIntimacao(ctx context.Context, tenantID, intimationID string) error
	ReopenIntimacao(ctx context.Context, tenantID, intimationID string) error
}

// reader is the narrow port the Handler uses from the read use case — the
// keyset-paginated screen reads (each returns the page plus whether more remain).
type reader interface {
	Processos(ctx context.Context, q ProcessosQuery) (ProcessosResult, error)
	Processo(ctx context.Context, tenantID, id string) (ProcessoView, error)
	Intimacoes(ctx context.Context, q IntimacoesQuery) (IntimacoesResult, error)
	Intimacao(ctx context.Context, tenantID, id string) (IntimacaoDetailView, error)
	Andamentos(ctx context.Context, q AndamentosQuery) (AndamentosResult, error)
	IntimacoesByProcesso(ctx context.Context, q IntimacoesByProcessoQuery) (IntimacoesByProcessoResult, error)
	Partes(ctx context.Context, tenantID, courtRecordID string) (PartesView, error)
	ProcessosSummary(ctx context.Context, tenantID string) (ProcessosSummaryView, error)
	IntimacoesSummary(ctx context.Context, tenantID string) (IntimacoesSummaryView, error)
	ImportStatus(ctx context.Context, tenantID string) (ImportStatusView, error)
	Reconciliations(ctx context.Context, tenantID string) (ReconciliationsView, error)
	ReconciliationDetail(ctx context.Context, tenantID, jobID string) (ReconciliationDetailView, error)
	SyncRunItems(ctx context.Context, tenantID, syncRunID string) (SyncRunItemsView, error)
}

// Handler is the acquisition HTTP surface. It owns its routing; the api only
// composes by calling RegisterV1.
type Handler struct {
	uc     handlerUC
	reader reader
}

// NewHandler wires the handler to the acquisition write and read use cases.
func NewHandler(uc handlerUC, reader reader) *Handler {
	return &Handler{uc: uc, reader: reader}
}

// RegisterV1 mounts acquisition's authenticated routes on the /v1 group. The
// write route is guarded by RequireRole(ADMIN); the reads are open to any
// authenticated principal of the tenant (scoped to its own rows).
func (h *Handler) RegisterV1(r fiber.Router) {
	r.Post("/acquisition/integrations", middleware.RequireRole(roleAdmin), h.activate)
	r.Get("/acquisition/integrations", h.list)
	r.Get("/acquisition/import-status", h.importStatus)
	r.Get("/acquisition/reconciliations", h.reconciliations)
	r.Get("/acquisition/reconciliations/:jobId", h.reconciliationDetail)
	r.Get("/acquisition/sync-runs/:syncRunId/items", h.syncRunItems)
	r.Get("/processos", h.listProcessos)
	// summary before :id so the static "/summary" segment is never shadowed by the param.
	r.Get("/processos/summary", h.processosSummary)
	r.Get("/processos/:id", h.getProcesso)
	r.Put("/processos/:id/responsavel", h.assignResponsible)
	r.Get("/processos/:id/andamentos", h.listAndamentos)
	r.Get("/processos/:id/intimacoes", h.listIntimacoesByProcesso)
	r.Get("/processos/:id/partes", h.listPartes)
	r.Get("/intimacoes", h.listIntimacoes)
	r.Get("/intimacoes/summary", h.intimacoesSummary)
	r.Get("/intimacoes/:id", h.getIntimacao)
	r.Post("/intimacoes/:id/resolve", h.resolveIntimacao)
	r.Post("/intimacoes/:id/ignore", h.ignoreIntimacao)
	r.Post("/intimacoes/:id/reopen", h.reopenIntimacao)
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
// activates every requested source under the shared scope (rows + events in one
// tx), and returns 201 with the activated integrations. tenant_id comes from the
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
	integrations, err := h.uc.ActivateIntegration(c.UserContext(), tenantID, req.Sources, req.Scope)
	if err != nil {
		return httpx.WriteError(c, err)
	}

	return c.Status(fiber.StatusCreated).JSON(newListEnvelope(integrations))
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
// paginated (?limit, ?cursor) and optionally filtered by ?search (cnj_number ILIKE).
// tenant_id comes from the principal.
func (h *Handler) listProcessos(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	limit := httpx.ClampLimit(c.QueryInt("limit"), httpx.DefaultLimit, httpx.MaxLimit)

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
		Search: c.Query("search"),
	})
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(newProcessosPage(res, limit))
}

// listIntimacoes handles GET /v1/intimacoes: the tenant's intimation inbox, newest
// availability first, keyset paginated (?limit, ?cursor) and optionally filtered by
// ?search (the court record's cnj_number ILIKE).
func (h *Handler) listIntimacoes(c *fiber.Ctx) error {
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

	res, err := h.reader.Intimacoes(c.UserContext(), IntimacoesQuery{
		TenantID: tenantID, LastMadeAvailable: lastMade, LastID: lastID, Limit: limit,
		Search: c.Query("search"),
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
// ProcessoView, same shape as a list row (including claim_value) — so it is returned
// without a list envelope. tenant_id comes from the principal, never the path/body: a
// miss or a foreign tenant's id is the read model's typed 404, a non-uuid id its typed 400.
func (h *Handler) getProcesso(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	view, err := h.reader.Processo(c.UserContext(), tenantID, c.Params("id"))
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
	return httpx.Page[ProcessoView]{Data: items, Page: meta}
}

// newIntimacoesPage wraps the intimações read model in the cursor envelope; the
// next cursor keys off the last row's (made_available_at, id) and the totals "X de Y".
func newIntimacoesPage(res IntimacoesResult, limit int) httpx.Page[IntimacaoView] {
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
	return httpx.Page[IntimacaoView]{Data: items, Page: meta}
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
	return httpx.Page[AndamentoView]{Data: items, Page: meta}
}

// newIntimacoesByProcessoPage wraps the per-process intimations read model in the cursor
// envelope; the next cursor keys off the last row's (made_available_at, id). There is no
// search on this tab, so the "X de Y" totals coincide (both the process's intimation count).
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
	return httpx.Page[IntimacaoView]{Data: items, Page: meta}
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

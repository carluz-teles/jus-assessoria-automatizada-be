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
}

// reader is the narrow port the Handler uses from the read use case — the
// keyset-paginated screen reads (each returns the page plus whether more remain).
type reader interface {
	Processos(ctx context.Context, q ProcessosQuery) ([]ProcessoView, bool, error)
	Intimacoes(ctx context.Context, q IntimacoesQuery) ([]IntimacaoView, bool, error)
	ImportStatus(ctx context.Context, tenantID string) (ImportStatusView, error)
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
	r.Get("/processos", h.listProcessos)
	r.Get("/intimacoes", h.listIntimacoes)
}

// Keyset sentinels for a first page (no cursor): the ascending processos scan
// starts below every row (” , zero uuid); the descending intimações scan starts
// above every row (max date, max uuid).
const (
	firstAscCNJ = ""
	zeroUUID    = "00000000-0000-0000-0000-000000000000"
	maxDate     = "9999-12-31"
	maxUUID     = "ffffffff-ffff-ffff-ffff-ffffffffffff"
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

// listProcessos handles GET /v1/processos: the tenant's live processes, keyset
// paginated (?limit, ?cursor). tenant_id comes from the principal.
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

	items, hasMore, err := h.reader.Processos(c.UserContext(), ProcessosQuery{
		TenantID: tenantID, LastCNJ: lastCNJ, LastID: lastID, Limit: limit,
	})
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(newProcessosPage(items, hasMore, limit))
}

// listIntimacoes handles GET /v1/intimacoes: the tenant's intimation inbox, newest
// availability first, keyset paginated (?limit, ?cursor).
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

	items, hasMore, err := h.reader.Intimacoes(c.UserContext(), IntimacoesQuery{
		TenantID: tenantID, LastMadeAvailable: lastMade, LastID: lastID, Limit: limit,
	})
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(newIntimacoesPage(items, hasMore, limit))
}

// newProcessosPage wraps the processos read model in the cursor envelope; the next
// cursor keys off the last row's (cnj_number, id).
func newProcessosPage(items []ProcessoView, hasMore bool, limit int) httpx.Page[ProcessoView] {
	if items == nil {
		items = []ProcessoView{}
	}
	meta := httpx.PageMeta{Limit: limit}
	if hasMore && len(items) > 0 {
		last := items[len(items)-1]
		tok := httpx.EncodeCursor(httpx.Cursor{LastID: last.ID, LastSortValue: last.CNJNumber})
		meta.NextCursor = &tok
	}
	return httpx.Page[ProcessoView]{Data: items, Page: meta}
}

// newIntimacoesPage wraps the intimações read model in the cursor envelope; the
// next cursor keys off the last row's (made_available_at, id).
func newIntimacoesPage(items []IntimacaoView, hasMore bool, limit int) httpx.Page[IntimacaoView] {
	if items == nil {
		items = []IntimacaoView{}
	}
	meta := httpx.PageMeta{Limit: limit}
	if hasMore && len(items) > 0 {
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

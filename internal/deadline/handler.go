package deadline

import (
	"context"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/httpx"
)

// handler.go is the deadline slice's HTTP read surface — the prazos screen reads that
// destravam the Prazos tab and the /prazos agenda. It is READ-ONLY at this slice: the
// creation path is event-driven (listener.go) and every mutation (confirm/met/missed/
// revoke) is a later fatia. The slice owns its routing; cmd/api only composes by calling
// RegisterV1. tenant_id always comes from the verified principal, never the path/query.

// reader is the narrow port the Handler uses from the read use case — the keyset-
// paginated prazos reads plus the single-prazo detail.
type reader interface {
	PrazosByProcesso(ctx context.Context, q PrazosByProcessoQuery) (PrazosByProcessoResult, error)
	Prazos(ctx context.Context, q PrazosQuery) (PrazosResult, error)
	Prazo(ctx context.Context, tenantID, id string) (PrazoDetailView, error)
}

// Handler is the deadline HTTP read surface. It owns its routing; the api only composes
// by calling RegisterV1.
type Handler struct {
	reader reader
}

// NewReadHandler wires the read handler to the prazos read use case. It is named for the
// read side deliberately: the slice's write surface (confirm/adjust, a later fatia) will
// mount its own routes without colliding with this.
func NewReadHandler(reader reader) *Handler {
	return &Handler{reader: reader}
}

// RegisterV1 mounts the prazos read routes on the /v1 group. All three are open to any
// authenticated principal of the tenant (scoped to its own prazos). The api calls this
// once — adding the slice's HTTP surface is one line of composition.
func (h *Handler) RegisterV1(r fiber.Router) {
	r.Get("/processos/:id/prazos", h.listPrazosByProcesso)
	r.Get("/prazos", h.listPrazos)
	r.Get("/prazos/:id", h.getPrazo)
}

// Keyset sentinels for a first page (no cursor): the ascending scan (soonest vencimento
// first) starts below every row — a date before any real prazo, and the zero uuid.
const (
	minDate  = "0001-01-01"
	zeroUUID = "00000000-0000-0000-0000-000000000000"
)

// listPrazosByProcesso handles GET /v1/processos/:id/prazos: one process's Prazos tab —
// its prazos ordered by end_date (soonest first), keyset paginated (?limit, ?cursor).
// The :id is the court_record id (the same id /processos returns); tenant_id comes from
// the principal, and the read is tenant-scoped so a foreign :id yields an empty page.
func (h *Handler) listPrazosByProcesso(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	limit := httpx.ClampLimit(c.QueryInt("limit"), httpx.DefaultLimit, httpx.MaxLimit)

	lastEnd, lastID := minDate, zeroUUID
	if tok := c.Query("cursor"); tok != "" {
		cur, err := httpx.DecodeCursor(tok)
		if err != nil {
			return httpx.WriteError(c, err)
		}
		lastEnd, lastID = cur.LastSortValue, cur.LastID
	}

	res, err := h.reader.PrazosByProcesso(c.UserContext(), PrazosByProcessoQuery{
		TenantID: tenantID, CourtRecordID: c.Params("id"),
		LastEnd: lastEnd, LastID: lastID, Limit: limit,
	})
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(newPrazosByProcessoPage(res, limit))
}

// listPrazos handles GET /v1/prazos: the tenant's agenda — every prazo ordered by
// end_date (soonest first), keyset paginated (?limit, ?cursor), optionally filtered by
// ?status (a closed set) and an end_date window ?from/?to (wire date 2006-01-02). A bad
// status or malformed date is a client error → 400.
func (h *Handler) listPrazos(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	limit := httpx.ClampLimit(c.QueryInt("limit"), httpx.DefaultLimit, httpx.MaxLimit)

	status := c.Query("status")
	if status != "" && !isKnownStatus(status) {
		return httpx.WriteError(c, apperr.NewInvalid("invalid status filter"))
	}
	from, err := validateOptionalDate(c.Query("from"))
	if err != nil {
		return httpx.WriteError(c, err)
	}
	to, err := validateOptionalDate(c.Query("to"))
	if err != nil {
		return httpx.WriteError(c, err)
	}

	lastEnd, lastID := minDate, zeroUUID
	if tok := c.Query("cursor"); tok != "" {
		cur, err := httpx.DecodeCursor(tok)
		if err != nil {
			return httpx.WriteError(c, err)
		}
		lastEnd, lastID = cur.LastSortValue, cur.LastID
	}

	res, err := h.reader.Prazos(c.UserContext(), PrazosQuery{
		TenantID: tenantID, Status: status, From: from, To: to,
		LastEnd: lastEnd, LastID: lastID, Limit: limit,
	})
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(newPrazosPage(res, limit))
}

// getPrazo handles GET /v1/prazos/:id: one prazo's audit detail. A miss (or a foreign
// tenant's id) is the repo's typed ErrDeadlineNotFound → 404. The view is the whole
// payload, so it is returned without a list envelope.
func (h *Handler) getPrazo(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	view, err := h.reader.Prazo(c.UserContext(), tenantID, c.Params("id"))
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(view)
}

// isKnownStatus reports whether s is a member of the closed prazo status set (the DB
// CHECK enforces the same set). An unknown ?status is rejected at the edge rather than
// silently returning an empty page.
func isKnownStatus(s string) bool {
	switch Status(s) {
	case StatusPending, StatusOpen, StatusMet, StatusMissed, StatusCancelled:
		return true
	default:
		return false
	}
}

// validateOptionalDate accepts an empty filter ("" → open bound) or a wire date
// (2006-01-02); anything else is a client error → 400. It returns the value verbatim so
// the repo re-parses it once, keeping the query struct wire-shaped.
func validateOptionalDate(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	if _, err := time.Parse(time.DateOnly, s); err != nil {
		return "", apperr.NewInvalid("invalid date filter (want YYYY-MM-DD)")
	}
	return s, nil
}

// newPrazosByProcessoPage wraps the Prazos-tab read model in the cursor envelope; the
// next cursor keys off the last row's (end_date, id). There is no filter on the tab, so
// the "X de Y" totals coincide (both the process's prazo count).
func newPrazosByProcessoPage(res PrazosByProcessoResult, limit int) httpx.Page[PrazoView] {
	items := res.Items
	if items == nil {
		items = []PrazoView{}
	}
	meta := httpx.PageMeta{Limit: limit, TotalCount: res.Total, Total: res.Total}
	if res.HasMore && len(items) > 0 {
		last := items[len(items)-1]
		tok := httpx.EncodeCursor(httpx.Cursor{
			LastID:        last.ID,
			LastSortValue: last.EndDate.Format(time.DateOnly),
		})
		meta.NextCursor = &tok
	}
	return httpx.Page[PrazoView]{Data: items, Page: meta}
}

// newPrazosPage wraps the agenda read model in the cursor envelope; the next cursor keys
// off the last row's (end_date, id) and the totals carry "X de Y" (filtered vs
// tenant-wide).
func newPrazosPage(res PrazosResult, limit int) httpx.Page[AgendaPrazoView] {
	items := res.Items
	if items == nil {
		items = []AgendaPrazoView{}
	}
	meta := httpx.PageMeta{Limit: limit, TotalCount: res.TotalCount, Total: res.Total}
	if res.HasMore && len(items) > 0 {
		last := items[len(items)-1]
		tok := httpx.EncodeCursor(httpx.Cursor{
			LastID:        last.ID,
			LastSortValue: last.EndDate.Format(time.DateOnly),
		})
		meta.NextCursor = &tok
	}
	return httpx.Page[AgendaPrazoView]{Data: items, Page: meta}
}

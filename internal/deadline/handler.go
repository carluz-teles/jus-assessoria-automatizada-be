package deadline

import (
	"context"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/httpx"
)

// handler.go is the deadline slice's HTTP surface — the prazos screen reads (the Prazos
// tab and the /prazos agenda) PLUS the F2 confirmation write (POST /prazos/confirm, the
// coração do produto, §9). The creation path stays event-driven (listener.go); PATCH/
// met/missed and task CRUD are later fatias. The slice owns its routing; cmd/api only
// composes by calling RegisterV1. tenant_id always comes from the verified principal,
// never the path/query/body.

// reader is the narrow port the Handler uses from the read use case — the keyset-
// paginated prazos reads plus the single-prazo detail.
type reader interface {
	PrazosByProcesso(ctx context.Context, q PrazosByProcessoQuery) (PrazosByProcessoResult, error)
	Prazos(ctx context.Context, q PrazosQuery) (PrazosResult, error)
	Prazo(ctx context.Context, tenantID, id string) (PrazoDetailView, error)
}

// writer is the narrow port the Handler uses from the write use case — the F2 confirmation
// PLUS the ajuste manual (PATCH) and the manual transitions (met/missed). It is deliberately
// separate from reader (and from the read use case): the writes run on the transactional
// write path, the reads on the pool.
type writer interface {
	Confirm(ctx context.Context, cmd ConfirmCommand) (ConfirmResult, error)
	Adjust(ctx context.Context, cmd AdjustCommand) (AdjustedDeadline, error)
	MarkMet(ctx context.Context, tenantID, deadlineID string) (MarkedDeadline, error)
	MarkMissed(ctx context.Context, tenantID, deadlineID string) (MarkedDeadline, error)
}

// Handler is the deadline HTTP surface. It owns its routing; the api only composes by
// calling RegisterV1.
type Handler struct {
	reader reader
	writer writer
}

// NewHandler wires the handler to the prazos read use case and the F2 confirmation write
// use case. Both are injected as narrow ports so the binary composes them (the api mounts
// this handler once) and tests substitute fakes.
func NewHandler(reader reader, writer writer) *Handler {
	return &Handler{reader: reader, writer: writer}
}

// RegisterV1 mounts the prazos routes on the /v1 group: three reads open to any
// authenticated principal of the tenant (scoped to its own prazos), the F2 write
// POST /prazos/confirm, and the ajuste + manual transitions of an already-derived prazo
// (PATCH /prazos/:id, POST /prazos/:id/met, POST /prazos/:id/missed). The api calls this
// once — adding the slice's HTTP surface is one line of composition.
func (h *Handler) RegisterV1(r fiber.Router) {
	r.Get("/processos/:id/prazos", h.listPrazosByProcesso)
	r.Get("/prazos", h.listPrazos)
	r.Get("/prazos/:id", h.getPrazo)
	r.Post("/prazos/confirm", h.confirmPrazo)
	r.Patch("/prazos/:id", h.adjustPrazo)
	r.Post("/prazos/:id/met", h.markPrazoMet)
	r.Post("/prazos/:id/missed", h.markPrazoMissed)
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

// confirmPrazo handles POST /v1/prazos/confirm: the F2 "Aprovar tudo" (§9). It validates
// the body, then confirms the prazo (PENDING→OPEN, recomputed) and creates the N tasks in
// one tx, returning 200 with the confirmed prazo + created tasks. tenant_id and
// confirmed_by come from the verified principal, never the body — so the write cannot be
// spoofed onto another tenant. A missing prazo for the intimação is the use case's typed
// ErrDeadlineNotFound → 404; a bad body is a 400 with the {kind,message,details} envelope.
func (h *Handler) confirmPrazo(c *fiber.Ctx) error {
	var req ConfirmRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteError(c, apperr.NewInvalid("malformed request body"))
	}
	if err := req.Validate(); err != nil {
		return httpx.WriteValidationError(c, err)
	}

	p, ok := httpx.PrincipalFromCtx(c)
	if !ok {
		// The route mounts under Auth, so this is defensive: no principal → treat as
		// unauthenticated rather than confirming with an empty tenant.
		return httpx.WriteError(c, apperr.NewUnauthorized("missing principal"))
	}

	res, err := h.writer.Confirm(c.UserContext(), req.toCommand(p.TenantID, p.UserID))
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(newConfirmResponse(res))
}

// adjustPrazo handles PATCH /v1/prazos/:id: the F2 ajuste manual (§9). It validates the
// partial body, then recomputes + updates the prazo in one tx, returning 200 with the
// recomputed prazo. tenant_id comes from the principal and the prazo id from the path (never
// the body). A miss is the use case's typed ErrDeadlineNotFound → 404; a terminal prazo is
// ErrDeadlineNotAdjustable → 409; a bad body is a 400 with the {kind,message,details} envelope.
func (h *Handler) adjustPrazo(c *fiber.Ctx) error {
	var req AdjustRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteError(c, apperr.NewInvalid("malformed request body"))
	}
	if err := req.Validate(); err != nil {
		return httpx.WriteValidationError(c, err)
	}

	p, ok := httpx.PrincipalFromCtx(c)
	if !ok {
		return httpx.WriteError(c, apperr.NewUnauthorized("missing principal"))
	}

	res, err := h.writer.Adjust(c.UserContext(), req.toAdjustCommand(p.TenantID, p.UserID, c.Params("id")))
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(newAdjustResponse(res))
}

// markPrazoMet handles POST /v1/prazos/:id/met: mark cumprido (§9), OPEN→MET. No body — the
// prazo id comes from the path and tenant_id from the principal. A miss is ErrDeadlineNotFound
// → 404; a non-OPEN prazo is ErrDeadlineNotOpen → 409.
func (h *Handler) markPrazoMet(c *fiber.Ctx) error {
	p, ok := httpx.PrincipalFromCtx(c)
	if !ok {
		return httpx.WriteError(c, apperr.NewUnauthorized("missing principal"))
	}
	res, err := h.writer.MarkMet(c.UserContext(), p.TenantID, c.Params("id"))
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(newMarkResponse(res))
}

// markPrazoMissed handles POST /v1/prazos/:id/missed: mark perdido (§9), OPEN→MISSED. The
// manual counterpart of the D+1 carência auto-miss (4b-ii) — same guard (OPEN only), same
// deadline.missed fact. A miss → 404; a non-OPEN prazo → 409.
func (h *Handler) markPrazoMissed(c *fiber.Ctx) error {
	p, ok := httpx.PrincipalFromCtx(c)
	if !ok {
		return httpx.WriteError(c, apperr.NewUnauthorized("missing principal"))
	}
	res, err := h.writer.MarkMissed(c.UserContext(), p.TenantID, c.Params("id"))
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(newMarkResponse(res))
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

// confirmResponse is the POST /prazos/confirm payload: the confirmed prazo + the created
// tasks. It is a purpose-built write DTO (not the read-model detail view — that is 5b's
// concern), so the F2 screen can render the result without a follow-up read.
type confirmResponse struct {
	Deadline confirmedDeadlineView `json:"deadline"`
	Tasks    []confirmedTaskView   `json:"tasks"`
}

// confirmedDeadlineView is the confirmed prazo in the response — the recomputed,
// human-approved fact with its auditable holidays_applied.
type confirmedDeadlineView struct {
	ID              string    `json:"id"`
	CourtRecordID   string    `json:"court_record_id"`
	IntimationID    string    `json:"intimation_id"`
	Kind            string    `json:"kind"`
	Days            int       `json:"days"`
	Counting        string    `json:"counting"`
	Doubled         bool      `json:"doubled"`
	DoubledReason   string    `json:"doubled_reason"`
	Status          string    `json:"status"`
	StartDate       time.Time `json:"start_date"`
	EndDate         time.Time `json:"end_date"`
	HolidaysApplied []string  `json:"holidays_applied"`
	ConfirmedBy     string    `json:"confirmed_by"`
}

// confirmedTaskView is one created task in the response, with its DB-assigned id. due_date
// is the wire date (omitted when the task has none); assignee_user_id is omitted when
// unassigned.
type confirmedTaskView struct {
	ID             string `json:"id"`
	DeadlineID     string `json:"deadline_id"`
	CourtRecordID  string `json:"court_record_id"`
	IntimationID   string `json:"intimation_id"`
	Title          string `json:"title"`
	Description    string `json:"description,omitempty"`
	Kind           string `json:"kind,omitempty"`
	DueDate        string `json:"due_date,omitempty"`
	Status         string `json:"status"`
	Source         string `json:"source"`
	AssigneeUserID string `json:"assignee_user_id,omitempty"`
}

// newConfirmResponse maps the use case's ConfirmResult to the client-facing payload. The
// holidays audit and the wire dates are formatted here; the Tasks slice is always
// initialized so an empty result serializes as [] rather than null.
func newConfirmResponse(res ConfirmResult) confirmResponse {
	d := res.Deadline
	tasks := make([]confirmedTaskView, 0, len(res.Tasks))
	for _, t := range res.Tasks {
		tv := confirmedTaskView{
			ID:             t.ID,
			DeadlineID:     t.DeadlineID,
			CourtRecordID:  t.CourtRecordID,
			IntimationID:   t.IntimationID,
			Title:          t.Title,
			Description:    t.Description,
			Kind:           t.Kind,
			Status:         string(t.Status),
			Source:         string(t.Source),
			AssigneeUserID: t.AssigneeUserID,
		}
		if t.DueDate != nil {
			tv.DueDate = t.DueDate.Format(time.DateOnly)
		}
		tasks = append(tasks, tv)
	}
	return confirmResponse{
		Deadline: confirmedDeadlineView{
			ID:              d.ID,
			CourtRecordID:   d.CourtRecordID,
			IntimationID:    d.IntimationID,
			Kind:            d.Kind,
			Days:            d.Days,
			Counting:        string(d.Counting),
			Doubled:         d.Doubled,
			DoubledReason:   d.DoubledReason,
			Status:          string(d.Status),
			StartDate:       d.StartDate,
			EndDate:         d.EndDate,
			HolidaysApplied: formatDates(d.HolidaysApplied),
			ConfirmedBy:     d.ConfirmedBy,
		},
		Tasks: tasks,
	}
}

// adjustedDeadlineView is the PATCH /v1/prazos/:id payload: the recomputed prazo (the ajuste
// touches only the deadline, so there are no tasks). It carries the same auditable
// holidays_applied + wire dates as the confirm view; Status is echoed unchanged (the ajuste
// never flips the lifecycle).
type adjustedDeadlineView struct {
	ID              string    `json:"id"`
	CourtRecordID   string    `json:"court_record_id"`
	Kind            string    `json:"kind"`
	Days            int       `json:"days"`
	Counting        string    `json:"counting"`
	Doubled         bool      `json:"doubled"`
	DoubledReason   string    `json:"doubled_reason"`
	Status          string    `json:"status"`
	StartDate       time.Time `json:"start_date"`
	EndDate         time.Time `json:"end_date"`
	HolidaysApplied []string  `json:"holidays_applied"`
}

// newAdjustResponse maps the use case's AdjustedDeadline to the client-facing payload; the
// holidays audit is formatted here (always a non-nil slice, so an empty audit is []).
func newAdjustResponse(d AdjustedDeadline) adjustedDeadlineView {
	return adjustedDeadlineView{
		ID:              d.ID,
		CourtRecordID:   d.CourtRecordID,
		Kind:            d.Kind,
		Days:            d.Days,
		Counting:        string(d.Counting),
		Doubled:         d.Doubled,
		DoubledReason:   d.DoubledReason,
		Status:          string(d.Status),
		StartDate:       d.StartDate,
		EndDate:         d.EndDate,
		HolidaysApplied: formatDates(d.HolidaysApplied),
	}
}

// markResponse is the POST /v1/prazos/:id/met | .../missed payload: the transitioned prazo id
// and its new status (MET / MISSED) — the minimal fact the F2 screen needs to reflect the flip.
type markResponse struct {
	DeadlineID string `json:"deadline_id"`
	Status     string `json:"status"`
}

// newMarkResponse maps the use case's MarkedDeadline to the client-facing payload.
func newMarkResponse(d MarkedDeadline) markResponse {
	return markResponse{DeadlineID: d.ID, Status: string(d.Status)}
}

// formatDates renders the holidays audit as wire dates (2006-01-02), always a non-nil
// slice so it serializes as [] not null.
func formatDates(days []time.Time) []string {
	out := make([]string, 0, len(days))
	for _, d := range days {
		out = append(out, d.Format(time.DateOnly))
	}
	return out
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

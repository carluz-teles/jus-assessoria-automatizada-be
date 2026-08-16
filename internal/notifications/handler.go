package notifications

import (
	"context"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/httpx"
)

// handler.go is the slice's HTTP surface: the in-app inbox the FE lists, badges and
// marks as read. The event-driven write side (listener.go) has no HTTP; this is the
// read/controller layer added in slice 2a. It owns its routing (Register); the api
// only composes. tenant_id AND user_id come from the verified principal (read state
// is per-user), never the body — so isolation cannot be spoofed.

// Keyset sentinels for the first page of the inbox (descending on created_at, id):
// the scan starts above every row — a far-future timestamp and the max uuid.
const (
	maxCreatedAt = "9999-12-31T23:59:59.999999999Z"
	maxUUID      = "ffffffff-ffff-ffff-ffff-ffffffffffff"
)

// notificationsListParams is the inbox route's query-param allowlist
// (docs/erd-backend.md §4e.3): a client typo or a param belonging to another route is
// rejected with 400 instead of being silently ignored.
var notificationsListParams = map[string]struct{}{
	"type": {}, "unread": {}, "limit": {}, "cursor": {},
}

// isKnownNotificationType accepts only the closed type set the filter exposes — the
// handler is the app-level CHECK on the ?type param (notification.type is a plain
// text column; the closed set lives in the entity constants).
func isKnownNotificationType(v string) bool {
	switch v {
	case TypeImportFinished, TypeNewAndamento, TypeDeadlineDueSoonAviso, TypeDeadlineMissedAviso, TypeTrialEndingSoonAviso:
		return true
	default:
		return false
	}
}

// reader is the narrow port the Handler drives from the read use case: the inbox
// list, the unread badge, and the per-user read receipts.
type reader interface {
	List(ctx context.Context, q ListNotificationsQuery) (NotificationsResult, error)
	UnreadCount(ctx context.Context, tenantID, userID string) (int, error)
	MarkRead(ctx context.Context, tenantID, userID, notificationID string) error
	MarkAllRead(ctx context.Context, tenantID, userID string) error
}

// subscriber is the narrow port the SSE stream drives: join a tenant's push channel
// and range its raw payloads until the context ends. The slice adapter is lib/pubsub
// over Redis — the same channel the in-app consumer publishes on (slice 2b), reused
// on the subscribe side. It mirrors pubsub.Subscriber structurally so the slice stays
// decoupled from the infra package (the same pattern as the domain's publisher port).
type subscriber interface {
	Subscribe(ctx context.Context, channel string) (<-chan []byte, error)
}

// Handler is the notifications HTTP surface (the in-app inbox plus the real-time SSE
// stream). It owns its routing; the api only composes by calling Register.
type Handler struct {
	reader    reader
	sub       subscriber
	heartbeat time.Duration
}

// Option configures the Handler at construction. The one knob today is the SSE
// heartbeat interval, injected so tests drive it deterministically.
type Option func(*Handler)

// WithHeartbeat overrides the SSE keep-alive ping interval (default
// defaultHeartbeat). Production keeps the default; tests inject a controlled value.
func WithHeartbeat(d time.Duration) Option {
	return func(h *Handler) { h.heartbeat = d }
}

// NewHandler wires the handler to the read use case and the pub/sub subscriber that
// backs the SSE stream, applying any options over the defaults.
func NewHandler(reader reader, sub subscriber, opts ...Option) *Handler {
	h := &Handler{reader: reader, sub: sub, heartbeat: defaultHeartbeat}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// unreadCountResponse is the badge payload: {"count": N}.
type unreadCountResponse struct {
	Count int `json:"count"`
}

// Register mounts notifications' authenticated routes on the /v1 group. Every route
// is scoped to the caller's tenant AND user, both read from the verified principal.
func (h *Handler) Register(r fiber.Router) {
	r.Get("/notifications", h.list)
	r.Get("/notifications/stream", h.stream)
	r.Get("/notifications/unread-count", h.unreadCount)
	r.Post("/notifications/:id/read", h.markRead)
	r.Post("/notifications/read-all", h.markAllRead)
}

// list handles GET /v1/notifications: the caller's in-app inbox, newest first, keyset
// paginated (?limit, ?cursor); ?unread=true filters to the ones this user has not
// read, ?type (a closed set) to one aviso type. A param outside the route's allowlist
// or an unknown type is a client error → 400. tenant_id and user_id come from the
// principal.
func (h *Handler) list(c *fiber.Ctx) error {
	if err := httpx.RejectUnknownParams(c, notificationsListParams); err != nil {
		return httpx.WriteError(c, err)
	}
	p, _ := httpx.PrincipalFromCtx(c)
	limit := httpx.ClampLimit(c.QueryInt("limit"), httpx.DefaultLimit, httpx.MaxLimit)

	typ := c.Query("type")
	if typ != "" && !isKnownNotificationType(typ) {
		return httpx.WriteError(c, apperr.NewInvalid("invalid type filter"))
	}

	lastCreated, lastID := maxCreatedAt, maxUUID
	if tok := c.Query("cursor"); tok != "" {
		cur, err := httpx.DecodeCursor(tok)
		if err != nil {
			return httpx.WriteError(c, err)
		}
		lastCreated, lastID = cur.LastSortValue, cur.LastID
	}

	res, err := h.reader.List(c.UserContext(), ListNotificationsQuery{
		TenantID:    p.TenantID,
		UserID:      p.UserID,
		LastCreated: lastCreated,
		LastID:      lastID,
		UnreadOnly:  c.QueryBool("unread"),
		Type:        typ,
		Limit:       limit,
	})
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(newNotificationsPage(res, limit))
}

// unreadCount handles GET /v1/notifications/unread-count: the badge count of avisos
// visible to the caller that they have not read.
func (h *Handler) unreadCount(c *fiber.Ctx) error {
	p, _ := httpx.PrincipalFromCtx(c)
	count, err := h.reader.UnreadCount(c.UserContext(), p.TenantID, p.UserID)
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(unreadCountResponse{Count: count})
}

// markRead handles POST /v1/notifications/:id/read: records the caller's read receipt
// for one aviso. An id that is not a visible aviso of the tenant is 404; a repeat is
// 204 (idempotent). Returns 204 No Content on success.
func (h *Handler) markRead(c *fiber.Ctx) error {
	p, _ := httpx.PrincipalFromCtx(c)
	if err := h.reader.MarkRead(c.UserContext(), p.TenantID, p.UserID, c.Params("id")); err != nil {
		return httpx.WriteError(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// markAllRead handles POST /v1/notifications/read-all: records the caller's read
// receipt for every visible aviso they have not read. Idempotent; returns 204.
func (h *Handler) markAllRead(c *fiber.Ctx) error {
	p, _ := httpx.PrincipalFromCtx(c)
	if err := h.reader.MarkAllRead(c.UserContext(), p.TenantID, p.UserID); err != nil {
		return httpx.WriteError(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// newNotificationsPage wraps the inbox read model in the cursor envelope; the next
// cursor keys off the last row's (created_at, id), and the filters block carries the
// selectable type set.
func newNotificationsPage(res NotificationsResult, limit int) httpx.Page[NotificationView] {
	items := res.Items
	if items == nil {
		items = []NotificationView{}
	}
	meta := httpx.PageMeta{Limit: limit}
	if res.HasMore && len(items) > 0 {
		last := items[len(items)-1]
		tok := httpx.EncodeCursor(httpx.Cursor{
			LastID:        last.ID,
			LastSortValue: last.CreatedAt.Format(time.RFC3339Nano),
		})
		meta.NextCursor = &tok
	}
	return httpx.Page[NotificationView]{Data: items, Page: meta, Filters: res.Filters.NonNil()}
}

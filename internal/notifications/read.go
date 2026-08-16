package notifications

import (
	"context"
	"time"

	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/httpx"
)

// read.go is the slice's read side: the in-app inbox screen reads (list, unread
// badge) and the per-USER read receipts (mark one / mark all). It is the HTTP
// counterpart to the event-driven write side (domain.go) — a DTO per query, off the
// write aggregate. Read state is per user (docs 2a decision): two members of the same
// escritório read independently, so the receipt lives in notification_read keyed by
// (aviso, user), never on the shared notification row.

// NotificationView is one row of the in-app inbox: the aviso plus THIS user's read
// state. Title/Body are the materialized in-app text ("" for EMAIL avisos, which
// render at send). Read is the per-user receipt (EXISTS in notification_read).
type NotificationView struct {
	ID        string         `json:"id"`
	Type      string         `json:"type"`
	Title     string         `json:"title"`
	Body      string         `json:"body"`
	Payload   map[string]any `json:"payload"`
	Read      bool           `json:"read"`
	CreatedAt time.Time      `json:"created_at"`
}

// ListNotificationsQuery carries the tenant+user scope, the keyset cursor (the last
// row's created_at and id), the unread-only filter, the optional type filter (a closed
// set, "" = all) and the page size. The handler fills the max sentinel
// (LastCreated/LastID) for the first page; the repo turns them into the descending
// keyset predicate.
type ListNotificationsQuery struct {
	TenantID    string
	UserID      string
	LastCreated string // RFC3339Nano sort value; max sentinel for the first page
	LastID      string
	UnreadOnly  bool
	Type        string // ?type: closed set (Type* consts); "" = all
	Limit       int
}

// NotificationsResult is a page of the inbox plus whether a further page exists. It
// wraps the raw items because the inbox's filter options (the closed type set) travel
// with the page for the envelope's chips block.
type NotificationsResult struct {
	Items   []NotificationView
	HasMore bool
	Filters httpx.Filters
}

// readRepo is the narrow read/receipt port the ReadUseCase drives. The list and the
// unread count run on the pool (barrier 1 = the tenant+user filter); the receipt
// writes take the caller's tx so they commit in the tenant's RLS scope (barrier 2).
type readRepo interface {
	ListNotifications(ctx context.Context, q ListNotificationsQuery) ([]NotificationView, error)
	CountUnread(ctx context.Context, tenantID, userID string) (int, error)
	NotificationVisibleTo(ctx context.Context, tx database.Tx, notificationID, tenantID, userID string) (bool, error)
	MarkRead(ctx context.Context, tx database.Tx, notificationID, tenantID, userID string) error
	MarkAllRead(ctx context.Context, tx database.Tx, tenantID, userID string) error
}

// ReadUseCase serves the in-app notifications screen: list / badge / mark-read. The
// reads are a keyset pagination policy over readRepo — it over-fetches one row to
// learn whether a next page exists, without a COUNT (the acquisition read molde). The
// mark-* run in a tenant-scoped unit of work (RLS in force). It depends on the
// readRepo and UnitOfWork interfaces, never a concrete implementation (docs 2.5).
type ReadUseCase struct {
	repo readRepo
	uow  database.UnitOfWork
}

// NewReadUseCase wires the read use case to its repository and unit of work (share
// the slice's repo with the writer).
func NewReadUseCase(repo readRepo, uow database.UnitOfWork) *ReadUseCase {
	return &ReadUseCase{repo: repo, uow: uow}
}

// List returns up to q.Limit avisos visible to the user (newest first) and whether a
// further page exists. It over-fetches one row so the handler learns hasMore without
// a separate COUNT. The envelope's filter options — the closed type set from the
// entity constants — are assembled alongside so the FE renders the chips without a
// second request.
func (uc *ReadUseCase) List(ctx context.Context, q ListNotificationsQuery) (NotificationsResult, error) {
	limit := q.Limit
	q.Limit = limit + 1
	rows, err := uc.repo.ListNotifications(ctx, q)
	if err != nil {
		return NotificationsResult{}, err
	}
	hasMore := false
	if len(rows) > limit {
		rows, hasMore = rows[:limit], true
	}
	f := httpx.Filters{}
	f.SetEnum("type", TypeImportFinished, TypeNewAndamento, TypeDeadlineDueSoonAviso, TypeDeadlineMissedAviso, TypeTrialEndingSoonAviso)
	return NotificationsResult{Items: rows, HasMore: hasMore, Filters: f}, nil
}

// UnreadCount returns the number of avisos visible to the user that they have not
// read — the in-app badge count.
func (uc *ReadUseCase) UnreadCount(ctx context.Context, tenantID, userID string) (int, error) {
	return uc.repo.CountUnread(ctx, tenantID, userID)
}

// MarkRead records the user's read receipt for one aviso, in the tenant scope. An id
// that is not a visible aviso of the tenant is ErrNotificationNotFound (→ 404); a
// second call is a no-op (the receipt insert is ON CONFLICT DO NOTHING).
func (uc *ReadUseCase) MarkRead(ctx context.Context, tenantID, userID, notificationID string) error {
	return uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		visible, err := uc.repo.NotificationVisibleTo(ctx, tx, notificationID, tenantID, userID)
		if err != nil {
			return err
		}
		if !visible {
			return ErrNotificationNotFound
		}
		return uc.repo.MarkRead(ctx, tx, notificationID, tenantID, userID)
	})
}

// MarkAllRead records read receipts for every aviso visible to the user that they
// have not read yet, in the tenant scope. Idempotent: a re-run inserts nothing.
func (uc *ReadUseCase) MarkAllRead(ctx context.Context, tenantID, userID string) error {
	return uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		return uc.repo.MarkAllRead(ctx, tx, tenantID, userID)
	})
}

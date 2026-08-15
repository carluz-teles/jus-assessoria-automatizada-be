package notifications

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jusassessoria/platform/internal/notifications/notificationsdb"
	"github.com/jusassessoria/platform/lib/database"
)

// InsertNotificationParams is the write DTO for the aviso. It is the domain's own
// shape (strings, a map) — the repo translates it to the sqlc/pgtype params at the
// boundary, so the use case never touches uuid.UUID or pgtype.*.
type InsertNotificationParams struct {
	TenantID        string
	RecipientUserID string // "" → SQL NULL
	Type            string
	Title           string // "" → SQL NULL (EMAIL avisos render text at send, not here)
	Body            string // "" → SQL NULL
	Payload         map[string]any
	Status          NotificationStatus
}

// InsertDeliveryParams is the write DTO for one channel delivery. ProviderMessageID
// and Error are empty at creation (QUEUED) — they are filled by UpdateDeliveryStatus
// once the send resolves — but present here for the create-FAILED-directly path.
type InsertDeliveryParams struct {
	NotificationID    string
	TenantID          string
	Channel           string
	Status            DeliveryStatus
	ProviderMessageID string
	Error             string
}

// UpdateDeliveryStatusParams is the write DTO recording a send's outcome, scoped by
// delivery id AND tenant id (app-layer barrier on top of RLS).
type UpdateDeliveryStatusParams struct {
	DeliveryID        string
	TenantID          string
	Status            DeliveryStatus
	ProviderMessageID string
	Error             string
}

// Repository is the persistence port the use case depends on (never the concrete
// impl). The write methods take the caller's tx so they participate in the use
// case's unit of work, and the recipient read runs inside it too so RLS scopes it to
// the event's tenant (docs §4b.1). FindDeliveryByProviderMessageID is the exception:
// a provider webhook carries no tenant, so that lookup runs on the pool (the billing
// FindByStripeCustomer molde) to recover the delivery and the tenant to scope by.
type Repository interface {
	InsertNotification(ctx context.Context, tx database.Tx, params InsertNotificationParams) (*Notification, error)
	InsertDelivery(ctx context.Context, tx database.Tx, params InsertDeliveryParams) (*NotificationDelivery, error)
	UpdateDeliveryStatus(ctx context.Context, tx database.Tx, params UpdateDeliveryStatusParams) (*NotificationDelivery, error)
	// FindRecipientEmail resolves an app_user's e-mail by internal id, scoped to the
	// tenant. A missing row is ErrRecipientNotFound, never (nil, nil).
	FindRecipientEmail(ctx context.Context, tx database.Tx, tenantID, appUserID string) (string, error)
	// FindDeliveryByProviderMessageID locates a delivery by the provider's message
	// id, on the pool (a resolution read, no tx — the webhook has no tenant). A
	// missing row is ErrDeliveryNotFound, never (nil, nil).
	FindDeliveryByProviderMessageID(ctx context.Context, providerMessageID string) (*NotificationDelivery, error)
	// HasRunningBackfillForTenant reports whether the tenant has a backfill_job still
	// RUNNING. Runs inside the caller's tx so RLS (plus the explicit tenant_id) scopes
	// it to the event's tenant — the in-app use case suppresses per-andamento avisos
	// while the onboarding import is in flight.
	HasRunningBackfillForTenant(ctx context.Context, tx database.Tx, tenantID string) (bool, error)

	// In-app inbox read side (slice 2a). The list and count run on the pool (barrier
	// 1 = the tenant+user filter, read state per user); the receipt writes take the
	// caller's tx so they commit in the tenant's RLS scope. Kept on the one Repository
	// so the slice shares a single repo across write and read use cases; readRepo is
	// the narrow subset the ReadUseCase depends on for testability.
	ListNotifications(ctx context.Context, q ListNotificationsQuery) ([]NotificationView, error)
	CountUnread(ctx context.Context, tenantID, userID string) (int, error)
	NotificationVisibleTo(ctx context.Context, tx database.Tx, notificationID, tenantID, userID string) (bool, error)
	MarkRead(ctx context.Context, tx database.Tx, notificationID, tenantID, userID string) error
	MarkAllRead(ctx context.Context, tx database.Tx, tenantID, userID string) error

	// Notification preferences (fatia 6). FindPreferenceChannels runs inside the
	// caller's tx — the OnNotificationRequested routing check joins the tenant scope
	// already open for the send; ListPreferences/UpsertPreference back the
	// GET/PUT /v1/notifications/preferences screen.
	//
	// FindPreferenceChannels resolves the saved override for (tenant, user, type).
	// A missing row is the typed ErrPreferenceNotFound, never (nil, nil) — the
	// caller reads that as "every channel enabled" (the default), not "none".
	FindPreferenceChannels(ctx context.Context, tx database.Tx, tenantID, appUserID, notifType string) ([]string, error)
	// ListPreferences reads the caller's own saved overrides on the pool (a screen
	// read, no tx). Types never touched are simply absent from the result.
	ListPreferences(ctx context.Context, tenantID, appUserID string) ([]NotificationPreference, error)
	// UpsertPreference saves the caller's full enabled-channel set for one type
	// inside the caller's tx. channels is the whole set, not a delta (ON CONFLICT
	// replaces it).
	UpsertPreference(ctx context.Context, tx database.Tx, tenantID, appUserID, notifType string, channels []string) (*NotificationPreference, error)
}

// pgRepository is the sqlc-backed implementation. q is bound to the pool for the
// tenant-less resolution read (FindDeliveryByProviderMessageID); every other method
// binds the generated code to the passed tx (all writes are transactional).
type pgRepository struct {
	q *notificationsdb.Queries
}

var (
	_ Repository = (*pgRepository)(nil)
	_ readRepo   = (*pgRepository)(nil)
)

// NewRepository binds the resolution read to pool (used only by the provider
// webhook). Inject a *pgxpool.Pool in production; both it and a mock satisfy
// notificationsdb.DBTX. The write methods ignore it — they bind to the tx they are
// given.
func NewRepository(pool notificationsdb.DBTX) Repository {
	return &pgRepository{q: notificationsdb.New(pool)}
}

// InsertNotification records the aviso inside the caller's tx. The tenant id parses
// to a uuid, the recipient id to a nullable pgtype.UUID (SQL NULL when empty), and
// the payload map to jsonb bytes.
func (r *pgRepository) InsertNotification(ctx context.Context, tx database.Tx, params InsertNotificationParams) (*Notification, error) {
	tid, err := uuid.Parse(params.TenantID)
	if err != nil {
		return nil, database.WrapInfra(err)
	}

	recipient, err := stringToPgUUID(params.RecipientUserID)
	if err != nil {
		return nil, err
	}

	payload, err := marshalPayload(params.Payload)
	if err != nil {
		return nil, err
	}

	row, err := notificationsdb.New(tx).InsertNotification(ctx, notificationsdb.InsertNotificationParams{
		TenantID:        tid,
		RecipientUserID: recipient,
		Type:            params.Type,
		Title:           textToNull(params.Title),
		Body:            textToNull(params.Body),
		Payload:         payload,
		Status:          string(params.Status),
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return notificationToEntity(row), nil
}

// InsertDelivery opens a channel delivery inside the caller's tx.
func (r *pgRepository) InsertDelivery(ctx context.Context, tx database.Tx, params InsertDeliveryParams) (*NotificationDelivery, error) {
	nid, err := uuid.Parse(params.NotificationID)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	tid, err := uuid.Parse(params.TenantID)
	if err != nil {
		return nil, database.WrapInfra(err)
	}

	row, err := notificationsdb.New(tx).InsertDelivery(ctx, notificationsdb.InsertDeliveryParams{
		NotificationID:    nid,
		TenantID:          tid,
		Channel:           params.Channel,
		Status:            string(params.Status),
		ProviderMessageID: textToNull(params.ProviderMessageID),
		Error:             textToNull(params.Error),
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return deliveryToEntity(row), nil
}

// UpdateDeliveryStatus records a send's outcome inside the caller's tx. A no-row
// result (an id from another tenant) maps to ErrDeliveryNotFound, never (nil, nil).
func (r *pgRepository) UpdateDeliveryStatus(ctx context.Context, tx database.Tx, params UpdateDeliveryStatusParams) (*NotificationDelivery, error) {
	did, err := uuid.Parse(params.DeliveryID)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	tid, err := uuid.Parse(params.TenantID)
	if err != nil {
		return nil, database.WrapInfra(err)
	}

	row, err := notificationsdb.New(tx).UpdateDeliveryStatus(ctx, notificationsdb.UpdateDeliveryStatusParams{
		ID:                did,
		TenantID:          tid,
		Status:            string(params.Status),
		ProviderMessageID: textToNull(params.ProviderMessageID),
		Error:             textToNull(params.Error),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrDeliveryNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return deliveryToEntity(row), nil
}

// FindRecipientEmail resolves the recipient's e-mail inside the caller's tx (so RLS
// scopes app_user to the tenant). A missing row is the typed ErrRecipientNotFound —
// the use case turns that into a FAILED delivery, never a dropped aviso.
func (r *pgRepository) FindRecipientEmail(ctx context.Context, tx database.Tx, tenantID, appUserID string) (string, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return "", database.WrapInfra(err)
	}
	uid, err := uuid.Parse(appUserID)
	if err != nil {
		return "", database.WrapInfra(err)
	}

	email, err := notificationsdb.New(tx).FindRecipientEmail(ctx, notificationsdb.FindRecipientEmailParams{
		ID:       uid,
		TenantID: tid,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrRecipientNotFound
	}
	if err != nil {
		return "", database.WrapInfra(err)
	}
	return email, nil
}

// FindPreferenceChannels reads inside the caller's tx (so RLS scopes it to the
// event's tenant, matching resolveEmail's pattern). A no-row result is the typed
// ErrPreferenceNotFound.
func (r *pgRepository) FindPreferenceChannels(ctx context.Context, tx database.Tx, tenantID, appUserID, notifType string) ([]string, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	uid, err := uuid.Parse(appUserID)
	if err != nil {
		return nil, database.WrapInfra(err)
	}

	channels, err := notificationsdb.New(tx).FindPreferenceChannels(ctx, notificationsdb.FindPreferenceChannelsParams{
		TenantID:  tid,
		AppUserID: uid,
		Type:      notifType,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrPreferenceNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return channels, nil
}

// ListPreferences reads the caller's saved overrides on the pool (a screen read, no
// tx). An empty result is an empty (never nil) slice, so the endpoint serializes
// data:[], not null.
func (r *pgRepository) ListPreferences(ctx context.Context, tenantID, appUserID string) ([]NotificationPreference, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	uid, err := uuid.Parse(appUserID)
	if err != nil {
		return nil, database.WrapInfra(err)
	}

	rows, err := r.q.ListPreferences(ctx, notificationsdb.ListPreferencesParams{
		TenantID:  tid,
		AppUserID: uid,
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	out := make([]NotificationPreference, 0, len(rows))
	for _, row := range rows {
		out = append(out, *preferenceToEntity(row))
	}
	return out, nil
}

// UpsertPreference saves the full channel set inside the caller's tx. RETURNING
// always yields a row (an upsert), so there is no not-found branch here.
func (r *pgRepository) UpsertPreference(ctx context.Context, tx database.Tx, tenantID, appUserID, notifType string, channels []string) (*NotificationPreference, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	uid, err := uuid.Parse(appUserID)
	if err != nil {
		return nil, database.WrapInfra(err)
	}

	row, err := notificationsdb.New(tx).UpsertPreference(ctx, notificationsdb.UpsertPreferenceParams{
		TenantID:  tid,
		AppUserID: uid,
		Type:      notifType,
		Channels:  channels,
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return preferenceToEntity(row), nil
}

// FindDeliveryByProviderMessageID reads the delivery bearing the provider's message
// id on the pool (a resolution read, no tx — the bounce/complaint webhook carries no
// tenant). A no-row result (an id we never sent, or already garbage-collected) maps
// to ErrDeliveryNotFound, which the webhook use case turns into an ack, never a
// (nil, nil).
func (r *pgRepository) FindDeliveryByProviderMessageID(ctx context.Context, providerMessageID string) (*NotificationDelivery, error) {
	row, err := r.q.FindDeliveryByProviderMessageID(ctx, textToNull(providerMessageID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrDeliveryNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return deliveryToEntity(row), nil
}

// HasRunningBackfillForTenant reports whether the tenant has a RUNNING backfill_job,
// inside the caller's tx (so RLS scopes the read to the tenant, alongside the explicit
// tenant_id). A driver fault is wrapped infra; the query itself never returns no-row
// (EXISTS always yields a bool).
func (r *pgRepository) HasRunningBackfillForTenant(ctx context.Context, tx database.Tx, tenantID string) (bool, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return false, database.WrapInfra(err)
	}

	running, err := notificationsdb.New(tx).HasRunningBackfillForTenant(ctx, tid)
	if err != nil {
		return false, database.WrapInfra(err)
	}
	return running, nil
}

// ListNotifications reads the user's in-app inbox (keyset-paginated, newest first) on
// the pool, filtered by tenant_id + per-user visibility (barrier 1). The caller
// passes the max sentinel cursor for the first page.
func (r *pgRepository) ListNotifications(ctx context.Context, q ListNotificationsQuery) ([]NotificationView, error) {
	tid, err := uuid.Parse(q.TenantID)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	uid, err := uuid.Parse(q.UserID)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	lastID, err := uuid.Parse(q.LastID)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	lastCreated, err := time.Parse(time.RFC3339Nano, q.LastCreated)
	if err != nil {
		return nil, database.WrapInfra(err)
	}

	rows, err := r.q.ListNotifications(ctx, notificationsdb.ListNotificationsParams{
		TenantID:    tid,
		Limit:       int32(q.Limit),
		UserID:      uid,
		UnreadOnly:  q.UnreadOnly,
		Type:        q.Type,
		LastCreated: pgtype.Timestamptz{Time: lastCreated, Valid: true},
		LastID:      lastID,
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}

	out := make([]NotificationView, 0, len(rows))
	for _, row := range rows {
		out = append(out, NotificationView{
			ID:        row.ID.String(),
			Type:      row.Type,
			Title:     derefString(row.Title),
			Body:      derefString(row.Body),
			Payload:   unmarshalPayload(row.Payload),
			Read:      row.Read,
			CreatedAt: row.CreatedAt.Time,
		})
	}
	return out, nil
}

// CountUnread returns how many avisos visible to the user carry no read receipt from
// them, on the pool, filtered by tenant_id (barrier 1).
func (r *pgRepository) CountUnread(ctx context.Context, tenantID, userID string) (int, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return 0, database.WrapInfra(err)
	}
	uid, err := uuid.Parse(userID)
	if err != nil {
		return 0, database.WrapInfra(err)
	}

	n, err := r.q.CountUnread(ctx, notificationsdb.CountUnreadParams{TenantID: tid, UserID: uid})
	if err != nil {
		return 0, database.WrapInfra(err)
	}
	return int(n), nil
}

// NotificationVisibleTo reports whether the aviso exists and is visible to the user
// in the tenant, inside the caller's tx (RLS in force). A non-uuid id references no
// aviso, so it is simply "not visible" — the use case turns that into 404 like any
// unknown id, never a 500.
func (r *pgRepository) NotificationVisibleTo(ctx context.Context, tx database.Tx, notificationID, tenantID, userID string) (bool, error) {
	nid, err := uuid.Parse(notificationID)
	if err != nil {
		return false, nil
	}
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return false, database.WrapInfra(err)
	}
	uid, err := uuid.Parse(userID)
	if err != nil {
		return false, database.WrapInfra(err)
	}

	visible, err := notificationsdb.New(tx).NotificationVisibleTo(ctx, notificationsdb.NotificationVisibleToParams{
		NotificationID: nid,
		TenantID:       tid,
		UserID:         uid,
	})
	if err != nil {
		return false, database.WrapInfra(err)
	}
	return visible, nil
}

// MarkRead records the user's read receipt for one aviso inside the caller's tx.
// Idempotent (ON CONFLICT DO NOTHING) — a repeat is a no-op, not an error.
func (r *pgRepository) MarkRead(ctx context.Context, tx database.Tx, notificationID, tenantID, userID string) error {
	nid, err := uuid.Parse(notificationID)
	if err != nil {
		return database.WrapInfra(err)
	}
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return database.WrapInfra(err)
	}
	uid, err := uuid.Parse(userID)
	if err != nil {
		return database.WrapInfra(err)
	}

	if err := notificationsdb.New(tx).MarkNotificationRead(ctx, notificationsdb.MarkNotificationReadParams{
		NotificationID: nid,
		UserID:         uid,
		TenantID:       tid,
	}); err != nil {
		return database.WrapInfra(err)
	}
	return nil
}

// MarkAllRead records read receipts for every aviso visible to the user that they
// have not read yet, inside the caller's tx. Idempotent — a re-run inserts nothing.
func (r *pgRepository) MarkAllRead(ctx context.Context, tx database.Tx, tenantID, userID string) error {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return database.WrapInfra(err)
	}
	uid, err := uuid.Parse(userID)
	if err != nil {
		return database.WrapInfra(err)
	}

	if err := notificationsdb.New(tx).MarkAllNotificationsRead(ctx, notificationsdb.MarkAllNotificationsReadParams{
		UserID:   uid,
		TenantID: tid,
	}); err != nil {
		return database.WrapInfra(err)
	}
	return nil
}

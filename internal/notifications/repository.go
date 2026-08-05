package notifications

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

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
}

// pgRepository is the sqlc-backed implementation. q is bound to the pool for the
// tenant-less resolution read (FindDeliveryByProviderMessageID); every other method
// binds the generated code to the passed tx (all writes are transactional).
type pgRepository struct {
	q *notificationsdb.Queries
}

var _ Repository = (*pgRepository)(nil)

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

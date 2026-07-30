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
// impl). Every method takes the caller's tx: the writes participate in the use
// case's unit of work, and the recipient read runs inside it too so RLS scopes it to
// the event's tenant (docs §4b.1).
type Repository interface {
	InsertNotification(ctx context.Context, tx database.Tx, params InsertNotificationParams) (*Notification, error)
	InsertDelivery(ctx context.Context, tx database.Tx, params InsertDeliveryParams) (*NotificationDelivery, error)
	UpdateDeliveryStatus(ctx context.Context, tx database.Tx, params UpdateDeliveryStatusParams) (*NotificationDelivery, error)
	// FindRecipientEmail resolves an app_user's e-mail by internal id, scoped to the
	// tenant. A missing row is ErrRecipientNotFound, never (nil, nil).
	FindRecipientEmail(ctx context.Context, tx database.Tx, tenantID, appUserID string) (string, error)
}

// pgRepository is the sqlc-backed implementation. It holds no pool: every query
// binds the generated code to the passed tx (all work is transactional).
type pgRepository struct{}

var _ Repository = (*pgRepository)(nil)

// NewRepository returns the sqlc-backed repository. It is stateless — each method
// binds the generated queries to the tx it is given.
func NewRepository() Repository { return &pgRepository{} }

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

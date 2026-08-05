package notifications

import (
	"encoding/json"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jusassessoria/platform/internal/notifications/notificationsdb"
	"github.com/jusassessoria/platform/lib/database"
)

// mapper.go is the boundary where driver types die (docs §4b.3): uuid.UUID,
// pgtype.* and the raw jsonb bytes are absorbed here so the entity stays pure. The
// repo returns *Notification / *NotificationDelivery, never the sqlc row.

// notificationToEntity maps a notification row to the entity, collapsing the
// nullable recipient uuid to a plain string and the jsonb payload to a map.
func notificationToEntity(r notificationsdb.Notification) *Notification {
	return &Notification{
		ID:              r.ID.String(),
		TenantID:        r.TenantID.String(),
		RecipientUserID: pgUUIDToString(r.RecipientUserID),
		Type:            r.Type,
		Title:           derefString(r.Title),
		Body:            derefString(r.Body),
		Payload:         unmarshalPayload(r.Payload),
		Status:          NotificationStatus(r.Status),
		CreatedAt:       r.CreatedAt.Time,
	}
}

// deliveryToEntity maps a delivery row to the entity, collapsing the nullable
// text columns (provider id, error) to plain strings.
func deliveryToEntity(r notificationsdb.NotificationDelivery) *NotificationDelivery {
	return &NotificationDelivery{
		ID:                r.ID.String(),
		NotificationID:    r.NotificationID.String(),
		TenantID:          r.TenantID.String(),
		Channel:           r.Channel,
		Status:            DeliveryStatus(r.Status),
		ProviderMessageID: derefString(r.ProviderMessageID),
		Error:             derefString(r.Error),
		CreatedAt:         r.CreatedAt.Time,
		UpdatedAt:         r.UpdatedAt.Time,
	}
}

// stringToPgUUID parses an id into a nullable pgtype.UUID: an empty string is SQL
// NULL (a tenant-level aviso with no recipient), any other value a valid uuid. A
// malformed non-empty id is an infra fault (the id came from a decoded event).
func stringToPgUUID(s string) (pgtype.UUID, error) {
	if s == "" {
		return pgtype.UUID{}, nil
	}
	id, err := uuid.Parse(s)
	if err != nil {
		return pgtype.UUID{}, database.WrapInfra(err)
	}
	return pgtype.UUID{Bytes: id, Valid: true}, nil
}

// pgUUIDToString collapses a nullable uuid column to a plain string, "" standing in
// for SQL NULL — recipient_user_id is null for a tenant-level aviso.
func pgUUIDToString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	return uuid.UUID(u.Bytes).String()
}

// derefString collapses a nullable text column (*string) to a plain string, an
// empty string standing in for SQL NULL.
func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// textToNull is the inverse: an empty string is written as SQL NULL, not "".
func textToNull(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// marshalPayload encodes the template data as jsonb bytes, a nil/empty map becoming
// the empty object so the column never holds JSON null.
func marshalPayload(payload map[string]any) ([]byte, error) {
	if len(payload) == 0 {
		return []byte("{}"), nil
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return b, nil
}

// unmarshalPayload decodes jsonb bytes back to a map. The bytes are our own round-
// trip, so a decode fault is not expected; it collapses to an empty map rather than
// failing a read (the map is display data, never a decision input).
func unmarshalPayload(b []byte) map[string]any {
	out := map[string]any{}
	if len(b) == 0 {
		return out
	}
	_ = json.Unmarshal(b, &out)
	return out
}

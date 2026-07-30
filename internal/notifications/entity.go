// Package notifications is the avisos domain: it CONSUMES the generic
// `notification.requested` event and DELIVERS the aviso to a recipient through a
// channel. v0 ships EMAIL only (via Resend); IN_APP/SMS are later channels. The
// provider (Resend) reports bounces and complaints back through a signed webhook,
// which flips the affected delivery to BOUNCED/COMPLAINED (see webhook.go).
//
// The producer of `notification.requested` is another slice — identity translating
// its own member_joined into a notification request. This slice never imports
// identity; it reacts to the event contract alone (docs §2.5: slices talk by event).
// Two aggregates model the work: a `notification` is the fact (someone should be told
// X), a `notification_delivery` is one per-channel attempt to tell them — so adding a
// channel adds a delivery row, not another notification.
package notifications

import "time"

// NotificationStatus is the lifecycle of the aviso itself, a text enum validated
// in the application (CHECK-on-app), not a DB enum. v0 only ever creates it; the
// per-channel progress lives on the delivery, not here.
type NotificationStatus string

// StatusCreated is the only notification status in v0 — the aviso is recorded and
// its delivery rows carry what happens next.
const StatusCreated NotificationStatus = "CREATED"

// DeliveryStatus is the lifecycle of a single channel delivery: QUEUED at creation,
// then SENT (the provider accepted it) or FAILED (it could not be sent). BOUNCED and
// COMPLAINED are the provider-webhook outcomes (slice 3d): the recipient's server
// permanently rejected the message, or the recipient marked it as spam. The status
// column is text + CHECK-on-app, so adding COMPLAINED needs no migration.
type DeliveryStatus string

const (
	DeliveryQueued     DeliveryStatus = "QUEUED"
	DeliverySent       DeliveryStatus = "SENT"
	DeliveryFailed     DeliveryStatus = "FAILED"
	DeliveryBounced    DeliveryStatus = "BOUNCED"
	DeliveryComplained DeliveryStatus = "COMPLAINED"
)

// Valid reports whether s is a known delivery status. The zero value ("") is
// invalid on purpose, so an unset status never silently passes as a real one.
func (s DeliveryStatus) Valid() bool {
	return s == DeliveryQueued || s == DeliverySent || s == DeliveryFailed ||
		s == DeliveryBounced || s == DeliveryComplained
}

// ChannelEmail is the channel value stored on a delivery and returned by the email
// Channel's Kind(). v0's only channel; kept a plain string const (not a type) so it
// does not clash with the Channel port interface.
const ChannelEmail = "EMAIL"

// Notification is the local aviso: someone (RecipientUserID, empty for a
// tenant-level aviso) should be told something (Type selects the template, Payload
// is its data). Its ID is the internal uuid. The delivery rows carry the outcome.
type Notification struct {
	ID              string
	TenantID        string
	RecipientUserID string // "" when the aviso is tenant-level (no single recipient)
	Type            string
	Payload         map[string]any
	Status          NotificationStatus
	CreatedAt       time.Time
}

// NotificationDelivery is one channel's attempt to deliver a Notification.
// ProviderMessageID is set once SENT (the provider's id — the key the bounce webhook
// correlates on); Error is set once FAILED, BOUNCED or COMPLAINED (the reason).
// CreatedAt/UpdatedAt bracket the attempt.
type NotificationDelivery struct {
	ID                string
	NotificationID    string
	TenantID          string
	Channel           string
	Status            DeliveryStatus
	ProviderMessageID string
	Error             string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

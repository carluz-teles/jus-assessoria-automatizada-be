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
	// DeliverySkipped — the recipient's notification_preference explicitly excluded
	// this channel for the aviso's type. Distinct from FAILED (nothing went wrong;
	// the user asked not to be reached this way) so the audit trail — and any future
	// "why didn't I get this e-mail" support flow — can tell the two apart.
	DeliverySkipped DeliveryStatus = "SKIPPED"
)

// Valid reports whether s is a known delivery status. The zero value ("") is
// invalid on purpose, so an unset status never silently passes as a real one.
func (s DeliveryStatus) Valid() bool {
	return s == DeliveryQueued || s == DeliverySent || s == DeliveryFailed ||
		s == DeliveryBounced || s == DeliveryComplained || s == DeliverySkipped
}

// Channel values stored on a delivery. ChannelEmail is the async-send channel (the
// EmailChannel port's Kind()); ChannelInApp is the in-app inbox channel (slice 1a),
// which persists a QUEUED delivery with no external send (real-time push is a later
// slice). Plain string consts (not a type) so they do not clash with the Channel port.
const (
	ChannelEmail = "EMAIL"
	ChannelInApp = "IN_APP"
)

// ValidChannel reports whether c is a known delivery channel. The empty string is
// invalid on purpose, so an unset channel never silently passes as a real one.
func ValidChannel(c string) bool {
	return c == ChannelEmail || c == ChannelInApp
}

// Notification type selectors materialized by the in-app use case (slice 1a). Each
// picks the fixed PT title/body the use case renders at write time; they are the
// aviso's `type` column, not an events.Event type.
const (
	// TypeImportFinished — the onboarding backfill closed (acquisition.backfill_finished).
	TypeImportFinished = "import_finished"
	// TypeNewAndamento — a new andamento was observed outside the backfill window
	// (acquisition.docket_entry_observed).
	TypeNewAndamento = "new_andamento"
	// TypeDeadlineDueSoonAviso — a prazo is approaching its vencimento (deadline.due_soon).
	// This is the aviso's `type` column, distinct from the dotted EVENT id
	// TypeDeadlineDueSoon in events.go, mirroring how TypeNewAndamento pairs with
	// TypeDocketEntryObserved.
	TypeDeadlineDueSoonAviso = "deadline_due_soon"
	// TypeDeadlineMissedAviso — a prazo was auto-marked MISSED at the D+1 carência
	// (deadline.missed).
	TypeDeadlineMissedAviso = "deadline_missed"
	// TypeTrialEndingSoonAviso — a tenant's trial is approaching its end
	// (billing.trial_ending_soon, fatia 2). This is the aviso's `type` column,
	// distinct from the dotted EVENT id TypeTrialEndingSoon in events.go.
	TypeTrialEndingSoonAviso = "trial_ending_soon"
	// TypePaymentFailedAviso — a Stripe invoice charge failed and the subscription
	// flipped to past_due (billing.payment_failed, fatia 6b). This is the aviso's
	// `type` column, distinct from the dotted EVENT id TypePaymentFailed in events.go.
	TypePaymentFailedAviso = "payment_failed"
	// TypeFilingSucceededAviso — a peça foi protocolada automaticamente no e-SAJ
	// (filing.succeeded). Aviso `type` column, distinto do dotted EVENT id
	// TypeFilingSucceeded em events.go.
	TypeFilingSucceededAviso = "filing_succeeded"
	// TypeFilingFailedAviso — a tentativa de protocolo automático no e-SAJ falhou
	// (filing.failed); o usuário deve protocolar manualmente.
	TypeFilingFailedAviso = "filing_failed"
	// TypeMemberJoinedAviso — a user joined the escritório (identity.member_joined),
	// delivered through the generic notification.requested/EMAIL path (NotifyUseCase),
	// not the in-app record() path the other Type*Aviso consts above go through. Named
	// here (not just left as a bare string on the producer side) so it can be validated
	// as a known type on PUT /v1/notifications/preferences — this slice never imports
	// identity, so the string is duplicated by value, not by reference; it must match
	// identity's own notifyTypeMemberJoined constant.
	TypeMemberJoinedAviso = "member_joined"
)

// validNotificationTypes is the closed set of aviso `type` values a preference can
// target — every Type*Aviso this slice knows about, whether it renders through the
// in-app record() path or the generic EMAIL path. ValidNotificationType is the
// PUT /v1/notifications/preferences guard: a typo'd type is a 400, not a silently
// saved dead preference that will never match a real aviso.
var validNotificationTypes = map[string]bool{
	TypeImportFinished:       true,
	TypeNewAndamento:         true,
	TypeDeadlineDueSoonAviso: true,
	TypeDeadlineMissedAviso:  true,
	TypeTrialEndingSoonAviso: true,
	TypePaymentFailedAviso:   true,
	TypeMemberJoinedAviso:    true,
	TypeFilingSucceededAviso: true,
	TypeFilingFailedAviso:    true,
}

// ValidNotificationType reports whether t is a known aviso type. The empty string
// is invalid on purpose, so an unset type never silently passes as a real one.
func ValidNotificationType(t string) bool {
	return validNotificationTypes[t]
}

// Notification is the local aviso: someone (RecipientUserID, empty for a
// tenant-level aviso) should be told something (Type selects the template, Payload
// is its data). Its ID is the internal uuid. The delivery rows carry the outcome.
type Notification struct {
	ID              string
	TenantID        string
	RecipientUserID string // "" when the aviso is tenant-level (no single recipient)
	Type            string
	// Title and Body are the materialized in-app text (slice 1a): set for IN_APP
	// avisos, "" (SQL NULL) for EMAIL avisos, whose channel renders text at send.
	Title   string
	Body    string
	Payload map[string]any
	Status  NotificationStatus
	// ReadAt lived here in slice 1a as a tenant-wide flag; read state is now per-user
	// (the notification_read table, migration 0018), so it is off the aggregate.
	CreatedAt time.Time

	// Severidade classifies the aviso for display/triage: info | atencao | critico
	// (migration 0097, the Alertas schema evolution). Defaults to "info" in the DB;
	// nothing in this slice sets it yet (a later fatia wires routing/severity).
	Severidade string
	// GroupKey clusters related avisos (e.g. same prazo firing more than once) so a
	// future UI can collapse them. "" stands in for SQL NULL (ungrouped).
	GroupKey string
	// ExpiresAt is when the aviso stops being actionable/relevant. nil stands in for
	// SQL NULL (no expiry).
	ExpiresAt *time.Time
	// SourceKind/SourceID identify the domain object that triggered the aviso (e.g.
	// "deadline"/<deadline id>). "" stands in for SQL NULL (no source recorded).
	SourceKind string
	SourceID   string
	// SourceEventID is the producing event's id, unique when set — the DB-level
	// idempotency floor for alert generation. "" stands in for SQL NULL.
	SourceEventID string
	// CourtCaseID links the aviso to a processo when applicable. "" stands in for
	// SQL NULL (no linked processo).
	CourtCaseID string
}

// NotificationPreference is one user's saved channel override for one aviso type
// (docs/erd-sistemas-auxiliares.md §6): Channels is the FULL enabled set for
// (TenantID, AppUserID, Type), not a delta. Its absence — no row for a given
// (user, type) — is NOT modeled here: the repository surfaces that as a typed
// not-found the routing use case reads as "every channel enabled" (the pre-slice
// default), so a tenant that never touches preferences behaves unchanged.
type NotificationPreference struct {
	ID        string
	TenantID  string
	AppUserID string
	Type      string
	Channels  []string
	UpdatedAt time.Time
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

	// Motivo records why this delivery ended up in its current state (e.g. a
	// human-readable dedup/suppression reason), distinct from Error which is
	// specifically the FAILED/BOUNCED/COMPLAINED failure reason. "" stands in for
	// SQL NULL (migration 0097, the Alertas schema evolution).
	Motivo string
	// SeenAt/ArchivedAt are per-delivery viewer state (seen in an inbox, archived by
	// the recipient). nil stands in for SQL NULL (not seen / not archived).
	SeenAt     *time.Time
	ArchivedAt *time.Time
}

// SeenMarker records the last time a user saw a given scope (e.g. an inbox or a
// processo's alert feed), so unread/new counts can be computed without a per-item
// read table. EscopoKind + EscopoID name the scope (e.g. "inbox"/"" or
// "court_case"/<id>); LastSeenAt is bumped every time the user opens that scope.
// One row per (TenantID, AppUserID, EscopoKind, EscopoID) — see the UNIQUE
// constraint in migration 0097.
type SeenMarker struct {
	ID         string
	TenantID   string
	AppUserID  string
	EscopoKind string
	EscopoID   string
	LastSeenAt time.Time
}

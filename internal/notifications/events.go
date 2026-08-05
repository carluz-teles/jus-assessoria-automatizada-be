package notifications

import (
	"github.com/jusassessoria/platform/internal/acquisition"
	"github.com/jusassessoria/platform/lib/events"
)

// The two acquisition-produced events this slice ALSO consumes (slice 1a). Vertical
// slice: slices talk only by event contract (docs §2.5), so notifications imports the
// contract — never acquisition's entity/repo — as a type alias. The import is acyclic
// (acquisition does not import notifications). The alias keeps the listener/use case
// speaking in notifications' own names while the shape and dotted id stay
// single-sourced in acquisition (no drift).
type (
	BackfillFinished    = acquisition.BackfillFinished
	DocketEntryObserved = acquisition.DocketEntryObserved
)

const (
	TypeBackfillFinished    = acquisition.TypeBackfillFinished
	TypeDocketEntryObserved = acquisition.TypeDocketEntryObserved
)

// TypeNotificationRequested is the dotted id this slice consumes. Its "notification"
// prefix routes it to the "notifications" work queue at the relay (lib/events'
// queueFor), so a slow email send never blocks court sync or AI work.
const TypeNotificationRequested = "notification.requested"

// NotificationRequested is the generic request-to-notify contract this slice
// consumes: WHO (RecipientUserID, in the tenant TenantID), WHAT template (Type,
// e.g. "member_joined") and its data (Payload). Base carries the event id (consumer
// dedup) and the aggregate id (the tenant).
//
// Type is a plain field (the template selector), not the events.Event Type() method:
// this struct is only ever DECODED here (events.Decode needs no interface), and the
// producer — a future slice — owns whatever type publishes it. Keeping Type as data
// is what lets one generic event drive every kind of aviso.
type NotificationRequested struct {
	events.Base
	TenantID        string         `json:"tenant_id"`
	RecipientUserID string         `json:"recipient_user_id"`
	Type            string         `json:"type"`
	Payload         map[string]any `json:"payload"`
}

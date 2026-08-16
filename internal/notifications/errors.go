package notifications

import "github.com/jusassessoria/platform/lib/apperr"

// Typed, HTTP-agnostic slice errors. This domain is async (a listener, not an HTTP
// edge), so the Kind mostly drives logs and the asynq retry decision rather than a
// status code. Absence is always a typed error from the repository, never (nil, nil).
var (
	// ErrRecipientNotFound — no app_user with the given id in the tenant, so the
	// aviso has no address to deliver to. The use case does NOT fail the listener on
	// this: it records a FAILED delivery and moves on (an unreachable recipient must
	// not wedge the queue).
	ErrRecipientNotFound = apperr.NewNotFound("notification recipient not found")

	// ErrDeliveryNotFound — a delivery-status update matched no row for the tenant
	// (an id from another tenant, or a deleted delivery). Typed, never (nil, nil).
	ErrDeliveryNotFound = apperr.NewNotFound("notification delivery not found")

	// ErrNotificationNotFound — a mark-read targeted an aviso that is not visible to
	// the caller in the tenant (an id from another tenant, one addressed to a
	// different user, or a garbage id). The read handler maps it to 404.
	ErrNotificationNotFound = apperr.NewNotFound("notification not found")

	// ErrTemplateNotFound — no email template is registered for the notification
	// type, so nothing can be rendered. A configuration fault, surfaced (not silently
	// sent empty) so the missing template is fixed.
	ErrTemplateNotFound = apperr.NewInvalid("no email template for notification type")

	// ErrPreferenceNotFound — no notification_preference row for (tenant, user,
	// type): the user never overrode this type. The routing use case reads this as
	// "every channel enabled" (the default), never as "every channel disabled" — a
	// missing row must never silently suppress delivery.
	ErrPreferenceNotFound = apperr.NewNotFound("notification preference not found")
)

// noRecipientEmailReason is the delivery.error text recorded when the recipient
// could not be resolved to an address — a human-readable reason, not a sentinel.
const noRecipientEmailReason = "recipient could not be resolved to an e-mail"

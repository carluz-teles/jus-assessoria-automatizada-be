package billing

import (
	"context"
	"time"
)

// Stripe webhook event types this slice reacts to. Kept as plain strings (not the
// SDK's typed constants) so the domain switches on them without importing
// stripe-go — the SDK lives only in the concrete gateway (stripe.go).
const (
	EventSubscriptionCreated = "customer.subscription.created"
	EventSubscriptionUpdated = "customer.subscription.updated"
	EventSubscriptionDeleted = "customer.subscription.deleted"
	EventPaymentFailed       = "invoice.payment_failed"
	EventCheckoutCompleted   = "checkout.session.completed"
)

// StripeGateway is the port over Stripe: it verifies inbound webhooks and reads
// the plan catalog. The domain depends on this interface only — the concrete
// implementation (stripeGateway) owns the stripe-go dependency, so entity.go /
// domain.go never import the SDK (docs §4b: the slice's core stays pure).
type StripeGateway interface {
	// VerifyWebhook checks the Stripe-Signature header against the signing secret
	// over the RAW body and decodes the event into the SDK-agnostic StripeEvent.
	// A bad signature returns ErrInvalidSignature; an undecodable object,
	// ErrMalformedEvent. Unknown event types verify fine and return a StripeEvent
	// carrying only Type/ID (the dispatch acks them).
	VerifyWebhook(payload []byte, sigHeader string) (StripeEvent, error)
	// ResolvePlan reads the plan and the ACTIVE-process ceiling from the Stripe
	// product behind a price id (the catalog lives in Stripe product metadata).
	// A missing/zero active_process_limit is ErrPlanUnresolved.
	ResolvePlan(ctx context.Context, priceID string) (plan string, activeProcessLimit int, err error)
}

// StripeEvent is the SDK-agnostic projection of a verified Stripe webhook the
// domain dispatches on. ID is the Stripe event id (the dedup key). TenantID is
// lifted from the object's metadata.tenant_id (set at checkout, fatia 4-B) — empty
// when absent, which the domain turns into ErrMissingTenant. Exactly one of
// Subscription / Invoice is set for the event types this slice models; both are
// nil for an unknown or no-op (checkout.session.completed) type.
type StripeEvent struct {
	ID           string
	Type         string
	TenantID     string
	Subscription *StripeSubscription
	Invoice      *StripeInvoice
}

// StripeSubscription is the slice of a Stripe subscription object the domain
// needs: the ids, the raw Stripe status, the price id (to resolve the plan) and
// the paid-through window. The gateway absorbs the SDK's shape (items[0].price,
// unix timestamps) into these fields.
type StripeSubscription struct {
	ID               string
	CustomerID       string
	Status           string
	PriceID          string
	CurrentPeriodEnd time.Time
}

// StripeInvoice is the slice of a Stripe invoice object the domain needs on a
// payment failure: the invoice id, the amount due, and the customer id (the
// fallback path to recover the tenant when the invoice carries no metadata).
type StripeInvoice struct {
	ID         string
	CustomerID string
	AmountDue  int64
}

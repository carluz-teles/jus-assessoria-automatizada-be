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

// StripeGateway is the port over Stripe: it verifies inbound webhooks, reads the
// plan catalog and — for the checkout/portal endpoints (fatia 4-B) — provisions
// customers and hosted sessions. The domain depends on this interface only; the
// concrete implementation (stripeGateway) owns the stripe-go dependency, so
// entity.go / domain.go never import the SDK (docs §4b: the slice's core stays pure).
type StripeGateway interface {
	// VerifyWebhook checks the Stripe-Signature header against the signing secret
	// over the RAW body and decodes the event into the SDK-agnostic StripeEvent.
	// A bad signature returns ErrInvalidSignature; an undecodable object,
	// ErrMalformedEvent. Unknown event types verify fine and return a StripeEvent
	// carrying only Type/ID (the dispatch acks them).
	VerifyWebhook(payload []byte, sigHeader string) (StripeEvent, error)
	// EnsureCustomer creates a Stripe Customer stamped with metadata.tenant_id so a
	// later webhook can recover the tenant from the customer→tenant mapping. The
	// caller (StartCheckout) reuses the customer id already stored on the tenant's
	// subscription when present and only calls this for a tenant that has none yet.
	EnsureCustomer(ctx context.Context, tenantID string) (customerID string, err error)
	// CreateCheckoutSession opens a hosted Stripe Checkout in subscription mode for
	// the customer/price and returns the redirect URL. It stamps tenant_id on the
	// subscription's metadata (subscription_data) so the 4-A webhook can scope the
	// projection — the checkout is the sole place that metadata is set. trialDays > 0
	// starts the subscription on a trial.
	CreateCheckoutSession(ctx context.Context, params CheckoutParams) (url string, err error)
	// CreatePortalSession opens a Stripe Billing Portal session for the customer and
	// returns the redirect URL the tenant manages its subscription through.
	CreatePortalSession(ctx context.Context, customerID, returnURL string) (url string, err error)
	// ListPlans reads the active plan catalog from Stripe (active recurring prices
	// with their product expanded). Each StripePlan carries the ACTIVE-process
	// ceiling from the product metadata; prices whose product lacks a usable limit
	// are skipped.
	ListPlans(ctx context.Context) ([]StripePlan, error)
}

// CheckoutParams is the input to CreateCheckoutSession — grouped into a struct
// because the call carries more than a handful of fields (options struct over a
// long positional signature). TenantID is stamped onto the subscription metadata,
// not shown to the customer; the URLs and TrialDays come from config.
type CheckoutParams struct {
	CustomerID string
	PriceID    string
	TenantID   string
	SuccessURL string
	CancelURL  string
	TrialDays  int
}

// StripePlan is the SDK-agnostic projection of a Stripe price+product the plans
// endpoint returns: the price id (the checkout key), the product name, the
// recurring amount and interval, and the ACTIVE-process ceiling read from the
// product metadata. It is built by the gateway so the domain/handler never touch
// stripe-go types. This is the RAW Stripe catalog ListPlans still reads live; it
// is distinct from the local Plan (entity.go/migration 0037), which is this BE's
// own source of truth for the plan catalog used everywhere else in the slice.
type StripePlan struct {
	PriceID            string
	Name               string
	Amount             int64
	Interval           string
	ActiveProcessLimit int
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

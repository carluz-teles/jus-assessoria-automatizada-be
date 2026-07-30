// Package billing is the vertical slice that projects a tenant's Stripe
// subscription locally (docs/erd-backend.md §4). Stripe is the source of truth
// for billing; this slice mirrors the subscription state by reacting to Stripe
// webhooks (customer.subscription.*, invoice.payment_failed) and emits domain
// events other slices consume — notably the v0 entitlement, the ceiling on
// ACTIVE processes (active_process_limit), resolved from the Stripe product.
//
// Stripe Customer = tenant, so there is exactly one subscription row per tenant.
package billing

import "time"

// Status is the subscription lifecycle, a text enum validated in the application
// (CHECK-on-app), not a DB enum. It is the subset of Stripe's subscription
// statuses the product cares about in v0; every other Stripe status collapses to
// StatusActive on a create/update (see normalizeStatus).
type Status string

const (
	StatusTrialing Status = "trialing"
	StatusActive   Status = "active"
	StatusPastDue  Status = "past_due"
	StatusCanceled Status = "canceled"
)

// Valid reports whether s is one of the known statuses. The zero value ("") is
// invalid on purpose, so an unset status never silently passes as a real one.
func (s Status) Valid() bool {
	return s == StatusTrialing || s == StatusActive || s == StatusPastDue || s == StatusCanceled
}

// Subscription is the local projection of a tenant's Stripe subscription. Its ID
// is the internal uuid; StripeCustomerID / StripeSubscriptionID bridge to Stripe.
// Plan and ActiveProcessLimit are the resolved entitlement (from the Stripe
// product metadata of the subscription's price) and are left zero on the
// status-only transitions (canceled, past_due) that carry no catalog data.
// CurrentPeriodEnd is a pointer because the column is nullable until a
// subscription.* event first sets the paid-through window.
type Subscription struct {
	ID                   string
	TenantID             string
	StripeCustomerID     string
	StripeSubscriptionID string
	Status               Status
	Plan                 string
	CurrentPeriodEnd     *time.Time
	ActiveProcessLimit   int
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

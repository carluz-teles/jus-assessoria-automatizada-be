// Package billing is the vertical slice that projects a tenant's Stripe
// subscription locally (docs/erd-backend.md §4). Stripe remains the source of
// truth for PAYMENT (charging, invoicing, the subscription lifecycle); this BE is
// the source of truth for the CATALOG (what a plan is — its process-count band
// and per-process price, see Plan/migration 0037) and for the trial policy (see
// TrialPolicy/migration 0038) — Stripe only supplies, per plan, the checkout price
// id. The slice mirrors the subscription state by reacting to Stripe webhooks
// (customer.subscription.*, invoice.payment_failed) and emits domain events other
// slices consume — notably the v0 entitlement, the ceiling on ACTIVE processes
// (active_process_limit), resolved by combining the local plan with the
// subscription (see effectiveEntitlement in domain.go).
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
//
// PlanID is the local plan (migration 0037) resolved from the Stripe price id —
// nil until the next customer.subscription.* event re-projects it (existing rows
// predate the local catalog). CustomPricePerProcessCents is the negotiated
// override for tenants priced outside the plan's table rate (Enterprise deals);
// nil means "use the plan's price_per_process_cents". TrialEndsAt is the trial
// window's end — nil outside a trial (fatia 2 populates it; this fatia only
// carries the column).
type Subscription struct {
	ID                         string
	TenantID                   string
	StripeCustomerID           string
	StripeSubscriptionID       string
	Status                     Status
	Plan                       string
	CurrentPeriodEnd           *time.Time
	ActiveProcessLimit         int
	CreatedAt                  time.Time
	UpdatedAt                  time.Time
	PlanID                     *string
	CustomPricePerProcessCents *int
	TrialEndsAt                *time.Time
}

// Plan is the local plan catalog (migration 0037) — the BE's own source of truth
// for what a plan is (its process-count band and per-process price), superseding
// the live Stripe product-metadata read this slice used to depend on for that
// data (see StripePlan for the raw Stripe-side projection ListPlans still uses).
// ID is the plan row's uuid — needed to stamp subscription.plan_id (the FK
// applySubscription writes on every projection). MaxProcesses nil means the plan
// has no ceiling (Enterprise); StripePriceID nil means the plan has no Stripe
// Checkout price wired yet.
type Plan struct {
	ID                   string
	Code                 string
	Name                 string
	MinProcesses         int
	MaxProcesses         *int
	PricePerProcessCents int
	StripePriceID        *string
	Active               bool
}

// TrialPolicy is a named trial configuration (migration 0038): how many days the
// trial runs and the active-process ceiling it grants while it lasts. Exactly one
// ACTIVE policy may have IsDefault=true (enforced by a partial unique index) — the
// one a fresh signup gets absent a specific policy assignment.
type TrialPolicy struct {
	Name               string
	IsDefault          bool
	TrialDays          int
	ActiveProcessLimit int
	Active             bool
}

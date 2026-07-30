package billing

import (
	"time"

	"github.com/google/uuid"

	"github.com/jusassessoria/platform/lib/events"
)

// Event contracts for the billing slice. Other slices may import these structs
// as the shape they consume — they are the only billing types allowed to cross a
// slice boundary (slices communicate by event, never by entity/repo). The
// gating slice (5-A) is the intended consumer of SubscriptionActivated/Updated/
// Canceled; the notifications slice, of PaymentFailed.
//
// Every billing event's aggregate is the tenant, so downstream ordering follows
// the tenant's stream. Base adds the event id (consumer dedup) and the aggregate
// id (the tenant); the exported payload fields are JSON-tagged to survive the
// outbox round-trip.

const (
	// TypeSubscriptionActivated — a subscription became active/trialing for the
	// first time (customer.subscription.created).
	TypeSubscriptionActivated = "billing.subscription_activated"
	// TypeSubscriptionUpdated — an existing subscription's plan or status changed
	// (customer.subscription.updated).
	TypeSubscriptionUpdated = "billing.subscription_updated"
	// TypeSubscriptionCanceled — a subscription was canceled
	// (customer.subscription.deleted).
	TypeSubscriptionCanceled = "billing.subscription_canceled"
	// TypePaymentFailed — an invoice payment failed (invoice.payment_failed); the
	// subscription is now past_due.
	TypePaymentFailed = "billing.payment_failed"

	aggregateTypeTenant = "tenant"
)

// SubscriptionActivated is emitted, in the same transaction as the projection
// write, when a tenant's subscription first becomes active/trialing. The payload
// carries the plan and the paid-through window so a consumer (gating) can grant
// entitlement without re-reading the subscription.
type SubscriptionActivated struct {
	events.Base
	TenantID         string    `json:"tenant_id"`
	Plan             string    `json:"plan"`
	CurrentPeriodEnd time.Time `json:"current_period_end"`
}

var _ events.Event = SubscriptionActivated{}

func (SubscriptionActivated) Type() string          { return TypeSubscriptionActivated }
func (SubscriptionActivated) AggregateType() string { return aggregateTypeTenant }

// SubscriptionUpdated is emitted, in the same transaction as the projection
// write, when an existing subscription's plan or status changes. The payload
// carries the current plan and status so a consumer can re-evaluate entitlement.
type SubscriptionUpdated struct {
	events.Base
	TenantID string `json:"tenant_id"`
	Plan     string `json:"plan"`
	Status   Status `json:"status"`
}

var _ events.Event = SubscriptionUpdated{}

func (SubscriptionUpdated) Type() string          { return TypeSubscriptionUpdated }
func (SubscriptionUpdated) AggregateType() string { return aggregateTypeTenant }

// SubscriptionCanceled is emitted, in the same transaction as the status flip,
// when a subscription is canceled. It carries only the tenant — a consumer
// revokes entitlement without needing the (now defunct) plan details.
type SubscriptionCanceled struct {
	events.Base
	TenantID string `json:"tenant_id"`
}

var _ events.Event = SubscriptionCanceled{}

func (SubscriptionCanceled) Type() string          { return TypeSubscriptionCanceled }
func (SubscriptionCanceled) AggregateType() string { return aggregateTypeTenant }

// PaymentFailed is emitted, in the same transaction as the past_due flip, when an
// invoice payment fails. The payload carries the failing invoice and the amount
// due so a consumer (notifications, dunning) can act without re-reading Stripe.
type PaymentFailed struct {
	events.Base
	TenantID  string `json:"tenant_id"`
	InvoiceID string `json:"invoice_id"`
	AmountDue int64  `json:"amount_due"`
}

var _ events.Event = PaymentFailed{}

func (PaymentFailed) Type() string          { return TypePaymentFailed }
func (PaymentFailed) AggregateType() string { return aggregateTypeTenant }

// newSubscriptionActivated builds the event for a first activation, minting a
// fresh v7 event id (time-ordered) as the aggregate/idempotency key carrier. A
// nil CurrentPeriodEnd (a subscription without a period, which should not happen
// on activation) collapses to the zero time rather than panicking.
func newSubscriptionActivated(s *Subscription) SubscriptionActivated {
	return SubscriptionActivated{
		Base:             events.Base{EventID: uuid.Must(uuid.NewV7()).String(), Aggregate: s.TenantID},
		TenantID:         s.TenantID,
		Plan:             s.Plan,
		CurrentPeriodEnd: derefTime(s.CurrentPeriodEnd),
	}
}

// newSubscriptionUpdated builds the event for a plan/status change on an existing
// subscription, minting a fresh v7 event id as the aggregate/idempotency key.
func newSubscriptionUpdated(s *Subscription) SubscriptionUpdated {
	return SubscriptionUpdated{
		Base:     events.Base{EventID: uuid.Must(uuid.NewV7()).String(), Aggregate: s.TenantID},
		TenantID: s.TenantID,
		Plan:     s.Plan,
		Status:   s.Status,
	}
}

// newSubscriptionCanceled builds the event for a cancellation, minting a fresh v7
// event id as the aggregate/idempotency key.
func newSubscriptionCanceled(tenantID string) SubscriptionCanceled {
	return SubscriptionCanceled{
		Base:     events.Base{EventID: uuid.Must(uuid.NewV7()).String(), Aggregate: tenantID},
		TenantID: tenantID,
	}
}

// newPaymentFailed builds the event for a failed invoice payment, minting a fresh
// v7 event id as the aggregate/idempotency key.
func newPaymentFailed(tenantID, invoiceID string, amountDue int64) PaymentFailed {
	return PaymentFailed{
		Base:      events.Base{EventID: uuid.Must(uuid.NewV7()).String(), Aggregate: tenantID},
		TenantID:  tenantID,
		InvoiceID: invoiceID,
		AmountDue: amountDue,
	}
}

// derefTime collapses a nullable time to a value, the zero time standing in for
// nil — used where an event field is a value time.Time but the entity's is
// optional.
func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

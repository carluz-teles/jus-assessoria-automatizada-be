package billing

import (
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/jusassessoria/platform/internal/identity"
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

// notifyTypePaymentFailed is the template selector carried in the notification's
// payload Type — it names the notifications-side e-mail template rendered for a
// failed charge. Matches notifications.TypePaymentFailedAviso by value (the two
// packages don't share a const: notifications never imports billing's internals,
// only the PaymentFailed event contract via type alias, docs §2.5).
const notifyTypePaymentFailed = "payment_failed"

// newPaymentFailedAdminNotification builds the notification.requested that fans
// the payment_failed e-mail out to ONE admin, minting a fresh v7 event id (time-
// ordered, distinct per admin) as the aggregate/idempotency key. Reuses
// identity.NotificationRequested — the generic request-to-notify contract — so
// billing turns a failed charge into an e-mail without importing the
// notifications package (which would drag its Resend/template machinery into
// every billing binary). Safe to import: billing already imports identity for
// TenantProvisioned (identity never imports billing back).
func newPaymentFailedAdminNotification(tenantID, adminID, invoiceID string, amountDue int64) identity.NotificationRequested {
	return identity.NotificationRequested{
		Base:            events.Base{EventID: uuid.Must(uuid.NewV7()).String(), Aggregate: tenantID},
		TenantID:        tenantID,
		RecipientUserID: adminID,
		NotifyType:      notifyTypePaymentFailed,
		Payload: map[string]any{
			"invoice_id": invoiceID,
			"amount_due": amountDue,
		},
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

// --- consumed: identity.tenant_provisioned (fatia 2) -------------------------

// TypeTenantProvisioned is the dotted id this slice CONSUMES. Only the const
// crosses the boundary from identity (the producer); the payload SHAPE is
// redefined LOCALLY as TenantProvisioned below, so this slice never imports
// identity's event struct (pattern (b), mirroring how the deadline/notifications
// slices consume each other's scheduled-check facts).
const TypeTenantProvisioned = identity.TypeTenantProvisioned

// TenantProvisioned is the LOCAL decode shape of identity.tenant_provisioned: the
// tenant a trial subscription should be started for. Only ever DECODED here
// (events.Decode needs no interface), so it carries no Type()/AggregateType().
// Base yields the event id this slice dedups the trial provisioning on — identity
// mints it as a STABLE key per tenant, so an at-least-once replay (a Clerk webhook
// redelivery) collapses to the SAME id here, never a second trial.
type TenantProvisioned struct {
	events.Base
	TenantID string `json:"tenant_id"`
}

// --- produced/consumed: the trial-ending-soon flow (fatia 2) -----------------

// trialEndingSoonLeadDays is how many days before trial_ends_at the scheduled
// check fires, and the threshold GetSubscription's trial_status read model uses
// to report "trial_ending_soon" — the same number drives both, so the check and
// the read model never disagree about what "soon" means.
const trialEndingSoonLeadDays = 2

const (
	// TypeTrialEndingSoonCheck is the dotted id of the INTERNAL, SCHEDULED
	// self-message this slice both PRODUCES (at trial start) and CONSUMES (at the
	// ETA, trialEndingSoonLeadDays before trial_ends_at). It carries no aviso
	// itself: when it fires the handler RE-READS the subscription's CURRENT state
	// and only THEN emits the real trial_ending_soon — so a trial converted or
	// canceled between provisioning and the mark never sends a stale aviso.
	TypeTrialEndingSoonCheck = "billing.trial_ending_soon_check"
	// TypeTrialEndingSoon is the dotted id of the REAL aviso this slice PRODUCES
	// when a trial_ending_soon_check fires on a subscription still trialing within
	// the window. Its consumer is notifications.
	TypeTrialEndingSoon = "billing.trial_ending_soon"
)

// TrialEndingSoonCheck is the trial's scheduled self-message, mirroring the
// deadline slice's DeadlineReminderCheck (see internal/deadline/events.go). Its
// idempotency key is the STABLE "trial-ending-soon:{subscription_id}" — it
// doubles as the asynq TaskID (schedule-once, the relay dedups a re-derive) and
// the fire handler's processed_event dedup key (process-once). TrialEndsAt is the
// value AT SCHEDULING TIME; the fire handler re-reads the subscription rather
// than trusting it, since it may be stale by the time the mark fires. processAt is
// UNEXPORTED so it never serializes — it only feeds ProcessAt() at publish time.
type TrialEndingSoonCheck struct {
	events.Base
	TenantID       string    `json:"tenant_id"`
	SubscriptionID string    `json:"subscription_id"`
	TrialEndsAt    time.Time `json:"trial_ends_at"`
	processAt      time.Time
}

var (
	_ events.Event          = TrialEndingSoonCheck{}
	_ events.ScheduledEvent = TrialEndingSoonCheck{}
)

func (TrialEndingSoonCheck) Type() string          { return TypeTrialEndingSoonCheck }
func (TrialEndingSoonCheck) AggregateType() string { return aggregateTypeTenant }

// ProcessAt opts the check INTO future delivery (docs erd-backend §4c; repo
// directive: ETA work is a scheduled task, not polling): start-of-day(trial_ends_at)
// minus trialEndingSoonLeadDays calendar days.
func (e TrialEndingSoonCheck) ProcessAt() (time.Time, bool) { return e.processAt, true }

// newTrialEndingSoonCheck builds the trial's single scheduled mark. aggregate_id
// is the tenant (every billing event's aggregate); the event id is the STABLE
// per-subscription key described on TrialEndingSoonCheck.
func newTrialEndingSoonCheck(tenantID, subscriptionID string, trialEndsAt time.Time) TrialEndingSoonCheck {
	return TrialEndingSoonCheck{
		Base:           events.Base{EventID: fmt.Sprintf("trial-ending-soon:%s", subscriptionID), Aggregate: tenantID},
		TenantID:       tenantID,
		SubscriptionID: subscriptionID,
		TrialEndsAt:    trialEndsAt,
		processAt:      startOfDay(trialEndsAt).AddDate(0, 0, -trialEndingSoonLeadDays),
	}
}

// TrialEndingSoon is the REAL aviso, emitted when a trial_ending_soon_check fires
// on a subscription re-checked as still trialing and inside the window. The
// payload is deliberately MINIMAL (TenantID, TrialEndsAt, DaysLeft) — no plan name,
// price or Stripe id: notifications does not need the catalog to render the
// e-mail, and this keeps the payload stable across catalog changes.
type TrialEndingSoon struct {
	events.Base
	TenantID    string    `json:"tenant_id"`
	TrialEndsAt time.Time `json:"trial_ends_at"`
	DaysLeft    int       `json:"days_left"`
}

var _ events.Event = TrialEndingSoon{}

func (TrialEndingSoon) Type() string          { return TypeTrialEndingSoon }
func (TrialEndingSoon) AggregateType() string { return aggregateTypeTenant }

// newTrialEndingSoon builds the real aviso from the re-checked subscription's
// state. Fresh uuid v7 event id (the consumer dedup key) — unlike the check, this
// is a genuinely new fact each time it fires, so it needs no stable key.
func newTrialEndingSoon(tenantID string, trialEndsAt time.Time, daysLeft int) TrialEndingSoon {
	return TrialEndingSoon{
		Base:        events.Base{EventID: uuid.Must(uuid.NewV7()).String(), Aggregate: tenantID},
		TenantID:    tenantID,
		TrialEndsAt: trialEndsAt,
		DaysLeft:    daysLeft,
	}
}

// startOfDay collapses a civil date to midnight in its own location — the anchor
// of the check's ETA math, mirroring the deadline slice's identical helper
// (internal/deadline/domain.go's startOfDay). trial_ends_at already carries no
// meaningful time-of-day component for this purpose, so this just normalizes it.
func startOfDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

// daysUntil returns the whole number of calendar days between now and t (t's own
// date minus now's), on start-of-day boundaries — negative once t's date is in
// the past. Shared by OnTrialEndingSoonCheck's re-check (domain.go) and the
// subscription read model's trial_status/days_until_trial_end (handler.go), so
// the two never disagree about what "N days left" means.
func daysUntil(t, now time.Time) int {
	return int(startOfDay(t).Sub(startOfDay(now)).Hours() / 24)
}

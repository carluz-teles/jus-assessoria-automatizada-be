package billing

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"time"

	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// processed_event consumer names this slice dedups under (docs §4c.3: dedup is
// per-consumer, so marking one event never blocks another consumer of the same
// event). consumerBilling is the webhook path (Stripe events); the other two are
// the async listener's (fatia 2), kept distinct from it and from each other so a
// tenant_provisioned replay and a trial_ending_soon_check redelivery dedup
// independently.
const (
	consumerBilling               = "billing"
	consumerBillingTrialProvision = "billing-trial-provision"
	consumerBillingTrialEndSoon   = "billing-trial-ending-soon"
)

// starterPlanCode is the plan a fresh trial starts on: the lowest paid tier
// (Starter). A pragmatic default until the product exposes plan choice at signup —
// it only pins which catalog row subscription.plan_id references; the trial's own
// ceiling (TrialPolicy.ActiveProcessLimit) is resolved independently and may
// differ from the Starter plan's own MaxProcesses.
const starterPlanCode = "starter"

// publisher is the narrow outbox port the use case needs — the producer half of
// the transactional outbox. *events.Outbox satisfies it structurally.
type publisher interface {
	Publish(ctx context.Context, tx database.Tx, ev events.Event) error
}

// deduper is the idempotency guard port. It marks (consumer, eventID) inside the
// caller's tx so the mark and the effect it guards commit together — a crash can
// never leave an event marked-but-not-applied. The billing adapter wraps
// events.Dedup bound to the tx.
type deduper interface {
	SeenOrMark(ctx context.Context, tx database.Tx, consumer, eventID string) (seen bool, err error)
}

// CheckoutConfig carries the tenant-facing redirect URLs and trial window the
// checkout/portal use cases need. It comes from process config (env), injected via
// WithCheckoutConfig — the webhook path (fatia 4-A) leaves it zero, the endpoints
// (fatia 4-B) set it. TrialDays 0 means no trial.
type CheckoutConfig struct {
	SuccessURL string
	CancelURL  string
	ReturnURL  string
	TrialDays  int
}

// UseCase carries the billing use cases: projecting the Stripe subscription state
// from verified webhooks (idempotent, at-least-once), emitting the domain events
// downstream slices consume, and driving the tenant-facing checkout/portal/plans
// endpoints. It depends on the Repository, StripeGateway, outbox publisher, deduper
// and UnitOfWork interfaces — never on a concrete implementation (docs §2.5).
type UseCase struct {
	repo     Repository
	gateway  StripeGateway
	outbox   publisher
	dedup    deduper
	uow      database.UnitOfWork
	checkout CheckoutConfig
	// now is the reference clock trial provisioning/read-model math compares
	// against; it defaults to time.Now and is overridable in tests via WithClock
	// (mirroring internal/deadline/domain.go's identical seam).
	now func() time.Time
	// adminLister resolves a tenant's admins for the payment_failed e-mail
	// fan-out (WithAdminLister). Optional: nil (the zero value) makes failPayment
	// skip the fan-out — the in-app tenant-level aviso still fires either way, so
	// omitting it degrades gracefully rather than failing the webhook.
	adminLister AdminLister
}

// Option configures optional UseCase collaborators. Kept variadic so the webhook
// wiring (fatia 4-A) constructs the use case unchanged while the api adds the
// checkout config for the endpoints (fatia 4-B).
type Option func(*UseCase)

// WithCheckoutConfig injects the redirect URLs and trial window the checkout and
// portal use cases redirect through.
func WithCheckoutConfig(c CheckoutConfig) Option {
	return func(uc *UseCase) { uc.checkout = c }
}

// WithClock overrides the reference clock trial provisioning (trial_ends_at) and
// the subscription read model (trial_status/days_until_trial_end) use. Production
// leaves the default (time.Now); tests pin it to assert deterministically.
func WithClock(now func() time.Time) Option {
	return func(uc *UseCase) { uc.now = now }
}

// WithAdminLister injects the port failPayment consults to fan out the
// payment_failed e-mail to every admin of the tenant. Omitted in tests/
// compositions that never need the fan-out (see AdminLister's doc).
func WithAdminLister(l AdminLister) Option {
	return func(uc *UseCase) { uc.adminLister = l }
}

// NewUseCase wires the use cases to their repository, Stripe gateway, outbox
// publisher, dedup guard and unit of work. Optional collaborators (the checkout
// config, the clock) arrive through Options so existing callers stay
// source-compatible; the clock defaults to time.Now.
func NewUseCase(repo Repository, gateway StripeGateway, outbox publisher, dedup deduper, uow database.UnitOfWork, opts ...Option) *UseCase {
	uc := &UseCase{repo: repo, gateway: gateway, outbox: outbox, dedup: dedup, uow: uow, now: time.Now}
	for _, opt := range opts {
		opt(uc)
	}
	return uc
}

// Now returns the use case's reference clock — the handler's read model
// (trial_status/days_until_trial_end) uses it so "now" is computed in exactly one
// place, never re-derived independently at the edge.
func (uc *UseCase) Now() time.Time { return uc.now() }

// HandleWebhook verifies a raw Stripe webhook and projects its effect. It is the
// whole entry point behind POST /webhooks/stripe: verify the signature over the
// RAW body, then dispatch by event type. A bad signature (ErrInvalidSignature)
// surfaces as a 400; every other typed error rides its own Kind to a status, and
// a non-2xx makes Stripe retry.
func (uc *UseCase) HandleWebhook(ctx context.Context, payload []byte, sigHeader string) error {
	ev, err := uc.gateway.VerifyWebhook(payload, sigHeader)
	if err != nil {
		return err
	}
	return uc.dispatch(ctx, ev)
}

// dispatch routes a verified event to the matching projection. The two write
// shapes are: a full upsert (created/updated — the object carries the plan) and a
// status-only flip (deleted/payment_failed — no catalog data). checkout.session.
// completed is a logged no-op (the subscription.* events carry the real state);
// any other type is acked so Stripe stops retrying it.
func (uc *UseCase) dispatch(ctx context.Context, ev StripeEvent) error {
	switch ev.Type {
	case EventSubscriptionCreated:
		return uc.applySubscription(ctx, ev, func(s *Subscription) events.Event { return newSubscriptionActivated(s) })

	case EventSubscriptionUpdated:
		return uc.applySubscription(ctx, ev, func(s *Subscription) events.Event { return newSubscriptionUpdated(s) })

	case EventSubscriptionDeleted:
		return uc.cancelSubscription(ctx, ev)

	case EventPaymentFailed:
		return uc.failPayment(ctx, ev)

	case EventCheckoutCompleted:
		// The subscription.* events carry the authoritative state; the checkout
		// session itself is a no-op we only record for observability.
		slog.InfoContext(ctx, "billing: checkout.session.completed acknowledged", "stripe_event_id", ev.ID)
		return nil

	default:
		return nil
	}
}

// applySubscription projects a customer.subscription.created/updated event: it
// resolves the LOCAL plan for the price id, then upserts the projection and
// publishes the caller-supplied event, ALL in one transaction. The plan lookup
// moved inside the tx (it used to run outside, before a Stripe HTTP call — the
// reason to keep it off a pooled connection — but it is now a local read, so it
// is just another statement in the same unit of work). A price id with no local
// plan is a catalog misconfiguration: fail loud with ErrPlanUnresolved rather than
// project a subscription with a zeroed entitlement. Dedup runs INSIDE the tx: a
// replay marks nothing new (seen=true) and returns before any write, so the
// projection and outbox are never duplicated.
func (uc *UseCase) applySubscription(ctx context.Context, ev StripeEvent, newEvent func(*Subscription) events.Event) error {
	if ev.TenantID == "" {
		return ErrMissingTenant
	}
	sub := ev.Subscription
	if sub == nil {
		return ErrMalformedEvent
	}

	return uc.uow.Do(ctx, ev.TenantID, func(tx database.Tx) error {
		seen, err := uc.dedup.SeenOrMark(ctx, tx, consumerBilling, ev.ID)
		if err != nil {
			return err
		}
		if seen {
			return nil
		}

		plan, err := uc.repo.FindPlanByStripePriceID(ctx, tx, sub.PriceID)
		if errors.Is(err, ErrPlanNotFound) {
			return ErrPlanUnresolved
		}
		if err != nil {
			return err
		}

		// No persisted subscription exists yet on a create, and this fatia's
		// webhook path never carries a custom price override — an empty probe
		// resolves the plan's own defaults, exactly effectiveEntitlement's
		// "no override" branch.
		limit, _ := effectiveEntitlement(&Subscription{}, plan)

		saved, err := uc.repo.UpsertSubscription(ctx, tx, UpsertParams{
			TenantID:             ev.TenantID,
			StripeCustomerID:     sub.CustomerID,
			StripeSubscriptionID: sub.ID,
			Status:               normalizeStatus(sub.Status),
			Plan:                 plan.Name,
			CurrentPeriodEnd:     sub.CurrentPeriodEnd,
			ActiveProcessLimit:   limit,
			PlanID:               &plan.ID,
		})
		if err != nil {
			return err
		}
		return uc.outbox.Publish(ctx, tx, newEvent(saved))
	})
}

// unlimitedActiveProcesses is the sentinel effectiveEntitlement returns for a
// plan with no process ceiling (Plan.MaxProcesses == nil — the Enterprise band).
// math.MaxInt32 keeps the value safely representable everywhere the entitlement
// is compared or stored (e.g. active_process_limit is an int32 column) while
// still comparing as "no tenant will ever hit this" against any real count.
const unlimitedActiveProcesses = math.MaxInt32

// effectiveEntitlement is the SOLE place that combines a subscription with its
// plan (and any negotiated override) into the entitlement actually enforced:
// activeProcessLimit is the plan's ceiling (unlimitedActiveProcesses when the
// plan has none); pricePerProcessCents is the tenant's negotiated override when
// set, else the plan's table rate. No other code in this slice (or its consumers)
// may re-derive this combination — extend this function instead.
func effectiveEntitlement(sub *Subscription, plan *Plan) (activeProcessLimit int, pricePerProcessCents int) {
	activeProcessLimit = unlimitedActiveProcesses
	if plan.MaxProcesses != nil {
		activeProcessLimit = *plan.MaxProcesses
	}

	pricePerProcessCents = plan.PricePerProcessCents
	if sub.CustomPricePerProcessCents != nil {
		pricePerProcessCents = *sub.CustomPricePerProcessCents
	}

	return activeProcessLimit, pricePerProcessCents
}

// cancelSubscription projects a customer.subscription.deleted event: it flips the
// status to canceled (leaving plan/limit intact for the record) and emits
// subscription_canceled in the same tx. Dedup-in-tx makes a replay a pure no-op.
func (uc *UseCase) cancelSubscription(ctx context.Context, ev StripeEvent) error {
	if ev.TenantID == "" {
		return ErrMissingTenant
	}

	return uc.uow.Do(ctx, ev.TenantID, func(tx database.Tx) error {
		seen, err := uc.dedup.SeenOrMark(ctx, tx, consumerBilling, ev.ID)
		if err != nil {
			return err
		}
		if seen {
			return nil
		}

		if _, err := uc.repo.UpdateSubscriptionStatus(ctx, tx, ev.TenantID, StatusCanceled); err != nil {
			return err
		}
		return uc.outbox.Publish(ctx, tx, newSubscriptionCanceled(ev.TenantID))
	})
}

// failPayment projects an invoice.payment_failed event: it flips the status to
// past_due and emits payment_failed in the same tx. The tenant comes from the
// event metadata when present; otherwise it is recovered from the stored
// customer→tenant mapping (FindByStripeCustomer) — invoices do not reliably carry
// the subscription's metadata. Dedup-in-tx makes a replay a pure no-op.
func (uc *UseCase) failPayment(ctx context.Context, ev StripeEvent) error {
	inv := ev.Invoice
	if inv == nil {
		return ErrMalformedEvent
	}

	tenantID, err := uc.resolveInvoiceTenant(ctx, ev)
	if err != nil {
		return err
	}

	return uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		seen, err := uc.dedup.SeenOrMark(ctx, tx, consumerBilling, ev.ID)
		if err != nil {
			return err
		}
		if seen {
			return nil
		}

		if _, err := uc.repo.UpdateSubscriptionStatus(ctx, tx, tenantID, StatusPastDue); err != nil {
			return err
		}
		if err := uc.outbox.Publish(ctx, tx, newPaymentFailed(tenantID, inv.ID, inv.AmountDue)); err != nil {
			return err
		}
		return uc.notifyAdmins(ctx, tx, tenantID, inv.ID, inv.AmountDue)
	})
}

// notifyAdmins fans the payment_failed e-mail out to every ADMIN of tenantID, in
// the SAME tx as the past_due flip and the payment_failed event above — so the
// fan-out is gated by the very same SeenOrMark that guards the rest of
// failPayment (a replayed Stripe webhook re-sends nothing). A nil adminLister (no
// composition wired one) is a no-op: the tenant-level in-app aviso (fatia 6b,
// internal/notifications) still fires either way, so the e-mail fan-out degrades
// gracefully instead of failing the webhook.
func (uc *UseCase) notifyAdmins(ctx context.Context, tx database.Tx, tenantID, invoiceID string, amountDue int64) error {
	if uc.adminLister == nil {
		return nil
	}

	adminIDs, err := uc.adminLister.ListTenantAdminIDs(ctx, tenantID)
	if err != nil {
		return err
	}
	for _, adminID := range adminIDs {
		if err := uc.outbox.Publish(ctx, tx, newPaymentFailedAdminNotification(tenantID, adminID, invoiceID, amountDue)); err != nil {
			return err
		}
	}
	return nil
}

// resolveInvoiceTenant returns the tenant an invoice belongs to: the metadata
// tenant_id when the checkout set it, else the tenant of the subscription stored
// under the invoice's Stripe customer id. A customer with no stored subscription
// yields ErrSubscriptionNotFound so Stripe retries (the create may still be in
// flight). This read runs on the pool, before the write tx opens.
func (uc *UseCase) resolveInvoiceTenant(ctx context.Context, ev StripeEvent) (string, error) {
	if ev.TenantID != "" {
		return ev.TenantID, nil
	}

	sub, err := uc.repo.FindByStripeCustomer(ctx, ev.Invoice.CustomerID)
	if errors.Is(err, ErrSubscriptionNotFound) {
		return "", ErrSubscriptionNotFound
	}
	if err != nil {
		return "", err
	}
	return sub.TenantID, nil
}

// StartCheckout opens a hosted Stripe Checkout for the tenant and returns its
// redirect URL. It refuses (409 ErrAlreadySubscribed) when the tenant already
// holds a live subscription (active/trialing) — a plan change goes through the
// portal, not a second checkout. It reuses the customer id stored on the tenant's
// subscription when present (e.g. re-subscribing after a cancellation) and only
// provisions a fresh Stripe Customer otherwise. No DB write happens here: the
// subscription row is projected later by the webhook, which reads the tenant_id
// this checkout stamps on the subscription metadata.
func (uc *UseCase) StartCheckout(ctx context.Context, tenantID, priceID string) (string, error) {
	sub, err := uc.repo.FindByTenant(ctx, tenantID)
	if err != nil && !errors.Is(err, ErrSubscriptionNotFound) {
		return "", err
	}
	if sub != nil && (sub.Status == StatusActive || sub.Status == StatusTrialing) {
		return "", ErrAlreadySubscribed
	}

	customerID := ""
	if sub != nil {
		customerID = sub.StripeCustomerID
	}
	if customerID == "" {
		customerID, err = uc.gateway.EnsureCustomer(ctx, tenantID)
		if err != nil {
			return "", err
		}
	}

	return uc.gateway.CreateCheckoutSession(ctx, CheckoutParams{
		CustomerID: customerID,
		PriceID:    priceID,
		TenantID:   tenantID,
		SuccessURL: uc.checkout.SuccessURL,
		CancelURL:  uc.checkout.CancelURL,
		TrialDays:  uc.checkout.TrialDays,
	})
}

// OpenPortal opens a Stripe Billing Portal session for the tenant and returns its
// redirect URL. The tenant must already have a Stripe customer (a subscription
// with a stored customer id); otherwise there is nothing to manage and it returns
// 404 ErrNoStripeCustomer.
func (uc *UseCase) OpenPortal(ctx context.Context, tenantID string) (string, error) {
	sub, err := uc.repo.FindByTenant(ctx, tenantID)
	if errors.Is(err, ErrSubscriptionNotFound) {
		return "", ErrNoStripeCustomer
	}
	if err != nil {
		return "", err
	}
	if sub.StripeCustomerID == "" {
		return "", ErrNoStripeCustomer
	}

	return uc.gateway.CreatePortalSession(ctx, sub.StripeCustomerID, uc.checkout.ReturnURL)
}

// GetSubscription returns the tenant's local subscription projection for the
// read-model endpoint. It reads the mirror this slice keeps — no Stripe call at
// request time (Stripe is the source of truth, the webhook keeps the mirror
// fresh). A tenant that never checked out gets ErrSubscriptionNotFound (→ 404).
func (uc *UseCase) GetSubscription(ctx context.Context, tenantID string) (*Subscription, error) {
	return uc.repo.FindByTenant(ctx, tenantID)
}

// OnTenantProvisioned handles one identity.tenant_provisioned (fatia 2): it
// starts the tenant's trial subscription. identity.ProvisionTenant publishes this
// event on EVERY UpsertTenant call (create AND replay — UpsertTenant is idempotent
// ON CONFLICT DO UPDATE, and detecting insert-vs-update on the producer side is
// unnecessary complexity), with a STABLE event id per tenant, so dedup here —
// exactly the at-least-once pattern billing already uses for Stripe webhooks — is
// what makes a replay a no-op instead of a second trial.
//
// Steps, all in one tx:
//  1. dedup — a replay marks nothing new and returns before any write;
//  2. resolve the default trial policy — no default configured is an operational
//     misconfiguration, so it fails LOUD (ErrNoDefaultTrialPolicy) rather than
//     silently starting a trial with no defined length;
//  3. resolve the Starter plan (the trial's default catalog row);
//  4. compute trial_ends_at = now + trial_policy.TrialDays;
//  5. upsert the subscription (trialing, the trial's OWN ActiveProcessLimit — NOT
//     the Starter plan's — trial_ends_at);
//  6. publish subscription_activated (reused, unchanged shape) + the scheduled
//     trial_ending_soon_check, both in the same tx.
func (uc *UseCase) OnTenantProvisioned(ctx context.Context, ev TenantProvisioned) error {
	return uc.uow.Do(ctx, ev.TenantID, func(tx database.Tx) error {
		seen, err := uc.dedup.SeenOrMark(ctx, tx, consumerBillingTrialProvision, ev.EventID)
		if err != nil {
			return err
		}
		if seen {
			return nil
		}

		policy, err := uc.repo.FindDefaultTrialPolicy(ctx, tx)
		if err != nil {
			// ErrNoDefaultTrialPolicy included: a missing default trial policy is a
			// catalog misconfiguration, never a silent "no trial" fallback.
			return err
		}

		plan, err := uc.repo.FindPlanByCode(ctx, tx, starterPlanCode)
		if err != nil {
			return err
		}

		trialEndsAt := uc.now().AddDate(0, 0, policy.TrialDays)

		saved, err := uc.repo.UpsertSubscription(ctx, tx, UpsertParams{
			TenantID:           ev.TenantID,
			Status:             StatusTrialing,
			Plan:               plan.Name,
			ActiveProcessLimit: policy.ActiveProcessLimit,
			PlanID:             &plan.ID,
			TrialEndsAt:        &trialEndsAt,
		})
		if err != nil {
			return err
		}

		if err := uc.outbox.Publish(ctx, tx, newSubscriptionActivated(saved)); err != nil {
			return err
		}
		return uc.outbox.Publish(ctx, tx, newTrialEndingSoonCheck(saved.TenantID, saved.ID, trialEndsAt))
	})
}

// OnTrialEndingSoonCheck handles one billing.trial_ending_soon_check (fatia 2):
// the trial's single scheduled mark fired at its ETA. It RE-READS the
// subscription's CURRENT state rather than trusting the check's payload — a trial
// converted (checkout completed) or canceled between provisioning and the mark
// must never send a stale "your trial is ending" aviso. Only a subscription STILL
// trialing, with trial_ends_at still inside the lead window, gets the real event;
// every other case (converted, canceled, or a trial_ends_at pushed further out
// than the check assumed) is a silent no-op.
//
// Steps:
//  1. dedup — a redelivered task returns before any write;
//  2. re-read the subscription by tenant;
//  3. status != trialing, or trial_ends_at unset/outside the window → no-op;
//  4. otherwise emit trial_ending_soon in the SAME tx.
func (uc *UseCase) OnTrialEndingSoonCheck(ctx context.Context, ev TrialEndingSoonCheck) error {
	return uc.uow.Do(ctx, ev.TenantID, func(tx database.Tx) error {
		seen, err := uc.dedup.SeenOrMark(ctx, tx, consumerBillingTrialEndSoon, ev.EventID)
		if err != nil {
			return err
		}
		if seen {
			return nil
		}

		sub, err := uc.repo.FindByTenant(ctx, ev.TenantID)
		if errors.Is(err, ErrSubscriptionNotFound) {
			// Should not happen (the mark is scheduled off a just-provisioned
			// subscription), but a gone row is nothing to warn about — never fail
			// loud on a stale mark.
			return nil
		}
		if err != nil {
			return err
		}
		if sub.Status != StatusTrialing || sub.TrialEndsAt == nil {
			// Converted (checkout completed), canceled, or — defensively — a row
			// with no trial window: nothing to warn about.
			return nil
		}

		daysLeft := daysUntil(*sub.TrialEndsAt, uc.now())
		if daysLeft < 0 || daysLeft > trialEndingSoonLeadDays {
			// Already expired, or trial_ends_at moved further out than the check
			// assumed (a policy/plan change extended the trial) — the CURRENT state
			// is outside the "ending soon" window, so stay silent.
			return nil
		}

		return uc.outbox.Publish(ctx, tx, newTrialEndingSoon(ev.TenantID, *sub.TrialEndsAt, daysLeft))
	})
}

// ListPlans returns the active plan catalog read LIVE from Stripe (StripePlan) —
// unchanged behavior for this fatia; migrating this endpoint onto the local Plan
// catalog is a follow-up out of scope here (see entity.go's package doc). It is a
// thin pass-through to the gateway, kept as a use case so the handler never
// touches the gateway and future filtering has a home.
func (uc *UseCase) ListPlans(ctx context.Context) ([]StripePlan, error) {
	return uc.gateway.ListPlans(ctx)
}

// normalizeStatus maps a raw Stripe subscription status onto the product's v0
// enum. trialing/past_due/canceled pass through; every other Stripe status
// (active, and the incomplete/unpaid/paused edge states) collapses to active —
// the subsequent subscription.updated corrects real transitions, and
// payment_failed is what drives past_due. Keeping the set small keeps the
// entitlement rule (5-A) simple.
func normalizeStatus(raw string) Status {
	switch Status(raw) {
	case StatusTrialing:
		return StatusTrialing
	case StatusPastDue:
		return StatusPastDue
	case StatusCanceled:
		return StatusCanceled
	default:
		return StatusActive
	}
}

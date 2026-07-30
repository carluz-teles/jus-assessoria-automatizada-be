package billing

import (
	"context"
	"errors"
	"log/slog"

	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// consumerBilling is the processed_event consumer name this slice dedups under.
// Each consumer dedups independently (docs §4c.3), so it is billing-specific.
const consumerBilling = "billing"

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

// NewUseCase wires the use cases to their repository, Stripe gateway, outbox
// publisher, dedup guard and unit of work. Optional collaborators (the checkout
// config) arrive through Options so existing callers stay source-compatible.
func NewUseCase(repo Repository, gateway StripeGateway, outbox publisher, dedup deduper, uow database.UnitOfWork, opts ...Option) *UseCase {
	uc := &UseCase{repo: repo, gateway: gateway, outbox: outbox, dedup: dedup, uow: uow}
	for _, opt := range opts {
		opt(uc)
	}
	return uc
}

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
// resolves the plan from Stripe, then upserts the projection and publishes the
// caller-supplied event in ONE transaction. ResolvePlan runs OUTSIDE the tx — it
// is an external HTTP call, which must not hold a pooled DB connection. Dedup runs
// INSIDE the tx: a replay marks nothing new (seen=true) and returns before any
// write, so the projection and outbox are never duplicated.
func (uc *UseCase) applySubscription(ctx context.Context, ev StripeEvent, newEvent func(*Subscription) events.Event) error {
	if ev.TenantID == "" {
		return ErrMissingTenant
	}
	sub := ev.Subscription
	if sub == nil {
		return ErrMalformedEvent
	}

	plan, limit, err := uc.gateway.ResolvePlan(ctx, sub.PriceID)
	if err != nil {
		return err
	}

	return uc.uow.Do(ctx, ev.TenantID, func(tx database.Tx) error {
		seen, err := uc.dedup.SeenOrMark(ctx, tx, consumerBilling, ev.ID)
		if err != nil {
			return err
		}
		if seen {
			return nil
		}

		saved, err := uc.repo.UpsertSubscription(ctx, tx, UpsertParams{
			TenantID:             ev.TenantID,
			StripeCustomerID:     sub.CustomerID,
			StripeSubscriptionID: sub.ID,
			Status:               normalizeStatus(sub.Status),
			Plan:                 plan,
			CurrentPeriodEnd:     sub.CurrentPeriodEnd,
			ActiveProcessLimit:   limit,
		})
		if err != nil {
			return err
		}
		return uc.outbox.Publish(ctx, tx, newEvent(saved))
	})
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
		return uc.outbox.Publish(ctx, tx, newPaymentFailed(tenantID, inv.ID, inv.AmountDue))
	})
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

// ListPlans returns the active plan catalog from Stripe. It is a thin pass-through
// to the gateway today (the catalog lives in Stripe), kept as a use case so the
// handler never touches the gateway and future filtering has a home.
func (uc *UseCase) ListPlans(ctx context.Context) ([]Plan, error) {
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

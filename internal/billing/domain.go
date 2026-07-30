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

// UseCase carries the billing use cases: projecting the Stripe subscription state
// from verified webhooks (idempotent, at-least-once) and emitting the domain
// events downstream slices consume. It depends on the Repository, StripeGateway,
// outbox publisher, deduper and UnitOfWork interfaces — never on a concrete
// implementation (docs §2.5).
type UseCase struct {
	repo    Repository
	gateway StripeGateway
	outbox  publisher
	dedup   deduper
	uow     database.UnitOfWork
}

// NewUseCase wires the use cases to their repository, Stripe gateway, outbox
// publisher, dedup guard and unit of work.
func NewUseCase(repo Repository, gateway StripeGateway, outbox publisher, dedup deduper, uow database.UnitOfWork) *UseCase {
	return &UseCase{repo: repo, gateway: gateway, outbox: outbox, dedup: dedup, uow: uow}
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

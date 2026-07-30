-- billing slice queries (subscription projection).
-- Writes are idempotent so the Stripe webhook (at-least-once) can replay safely;
-- the consumer-side dedup (processed_event, keyed by the Stripe event id) is the
-- authoritative guard, these upserts are the second line of defence.

-- name: UpsertSubscription :one
-- Project the full subscription state from a customer.subscription.created/updated
-- webhook. Keyed on tenant_id (Stripe Customer = tenant, one row per tenant): the
-- first sighting inserts, every later one refreshes the mutable fields. plan and
-- active_process_limit come resolved from the price's Stripe product metadata.
INSERT INTO subscription (
    tenant_id, stripe_customer_id, stripe_subscription_id,
    status, plan, current_period_end, active_process_limit
) VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (tenant_id) DO UPDATE
   SET stripe_customer_id     = EXCLUDED.stripe_customer_id,
       stripe_subscription_id = EXCLUDED.stripe_subscription_id,
       status                 = EXCLUDED.status,
       plan                   = EXCLUDED.plan,
       current_period_end     = EXCLUDED.current_period_end,
       active_process_limit   = EXCLUDED.active_process_limit,
       updated_at             = now()
RETURNING *;

-- name: UpdateSubscriptionStatus :one
-- Flip only the lifecycle status (a customer.subscription.deleted → canceled, an
-- invoice.payment_failed → past_due). It leaves plan / active_process_limit /
-- current_period_end untouched: those events carry no catalog data, so a status-only
-- write must not wipe the projected entitlement. Scoped by tenant_id (WHERE + RLS).
-- No row (a status event racing ahead of subscription.created) → the caller maps
-- pgx.ErrNoRows to a typed not-found so Stripe retries.
UPDATE subscription
   SET status = $2,
       updated_at = now()
 WHERE tenant_id = $1
RETURNING *;

-- name: FindByStripeCustomer :one
-- Resolve the stored subscription (and thus its tenant) by Stripe customer id.
-- Used to recover the tenant for an invoice.payment_failed whose object carries no
-- tenant_id metadata — the customer→tenant mapping was persisted at subscription
-- creation. No row → typed not-found (the caller retries).
SELECT * FROM subscription
WHERE stripe_customer_id = $1;

-- billing slice queries (subscription projection + local plan/trial catalog).
-- Writes are idempotent so the Stripe webhook (at-least-once) can replay safely;
-- the consumer-side dedup (processed_event, keyed by the Stripe event id) is the
-- authoritative guard, these upserts are the second line of defence.

-- name: UpsertSubscription :one
-- Project the full subscription state from a customer.subscription.created/updated
-- webhook. Keyed on tenant_id (Stripe Customer = tenant, one row per tenant): the
-- first sighting inserts, every later one refreshes the mutable fields. plan_id is
-- the LOCAL plan resolved from the price id (migration 0037+); active_process_limit
-- stays the domain-resolved entitlement (effectiveEntitlement). custom_price_per_
-- process_cents/trial_ends_at are NOT sourced from the webhook in this fatia — a
-- replay must never clobber a negotiated override or trial window some other path
-- already set, hence COALESCE(EXCLUDED, current) instead of a blind overwrite.
INSERT INTO subscription (
    tenant_id, stripe_customer_id, stripe_subscription_id,
    status, plan, current_period_end, active_process_limit,
    plan_id, custom_price_per_process_cents, trial_ends_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (tenant_id) DO UPDATE
   SET stripe_customer_id             = EXCLUDED.stripe_customer_id,
       stripe_subscription_id         = EXCLUDED.stripe_subscription_id,
       status                         = EXCLUDED.status,
       plan                           = EXCLUDED.plan,
       current_period_end             = EXCLUDED.current_period_end,
       active_process_limit           = EXCLUDED.active_process_limit,
       plan_id                        = EXCLUDED.plan_id,
       custom_price_per_process_cents = COALESCE(EXCLUDED.custom_price_per_process_cents, subscription.custom_price_per_process_cents),
       trial_ends_at                  = COALESCE(EXCLUDED.trial_ends_at, subscription.trial_ends_at),
       updated_at                     = now()
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

-- name: FindByTenant :one
-- Read the tenant's own subscription projection (Stripe Customer = tenant, one row
-- per tenant). Backs the read-model endpoint (GET /v1/billing/subscription) and the
-- checkout/portal flows that need the stored stripe_customer_id. Scoped by tenant_id
-- (WHERE + RLS). No row → the caller maps pgx.ErrNoRows to a typed not-found.
SELECT * FROM subscription
WHERE tenant_id = $1;

-- name: FindByStripeCustomer :one
-- Resolve the stored subscription (and thus its tenant) by Stripe customer id.
-- Used to recover the tenant for an invoice.payment_failed whose object carries no
-- tenant_id metadata — the customer→tenant mapping was persisted at subscription
-- creation. No row → typed not-found (the caller retries).
SELECT * FROM subscription
WHERE stripe_customer_id = $1;

-- name: FindPlanByStripePriceID :one
-- Resolve the local plan behind a Stripe price id — the webhook projection's
-- source of the plan catalog now that the BE (not a live Stripe read) owns it. No
-- row → typed not-found (the price is not mapped to any local plan yet, a catalog
-- misconfiguration the caller must not silently paper over).
SELECT * FROM plan
WHERE stripe_price_id = $1 AND active;

-- name: FindPlanByID :one
-- Resolve a plan by its own id — the entitlement adapter's lookup for the plan a
-- subscription references (subscription.plan_id). Deliberately NOT filtered by
-- active: deactivating a plan for new sign-ups must not silently strip the
-- entitlement of tenants already subscribed to it. No row → typed not-found (a
-- referenced plan should always exist; a miss is a data fault).
SELECT * FROM plan
WHERE id = $1;

-- name: FindPlanByCode :one
-- Resolve a plan by its stable code (starter | growth | scale | enterprise) — used
-- to resolve the plan a trial starts on. No row → typed not-found.
SELECT * FROM plan
WHERE code = $1 AND active;

-- name: ListActivePlans :many
-- The active plan catalog, ordered by the process band it prices (min_processes) —
-- the natural reading order for a plans listing.
SELECT * FROM plan
WHERE active
ORDER BY min_processes;

-- name: FindDefaultTrialPolicy :one
-- The single ACTIVE default trial policy (migration 0038's partial unique index
-- guarantees at most one exists). No row → typed not-found: a tenant cannot start
-- a trial when the catalog has no default configured.
SELECT * FROM trial_policy
WHERE is_default AND active;

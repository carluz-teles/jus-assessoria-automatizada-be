-- 0006_billing.up.sql — the local projection of a tenant's Stripe subscription
-- (billing fatia 4-A). Faithful to docs/erd-modelo-de-dados.md (source of truth
-- for the schema). Stripe is the source of truth for billing; this table mirrors
-- the subscription state reacting to Stripe webhooks (customer.subscription.*,
-- invoice.payment_failed). Stripe Customer = tenant, so exactly one row per tenant.
--
-- status is text + CHECK-on-app (trialing|active|past_due|canceled), the same
-- convention as the other enums. active_process_limit is the v0 entitlement —
-- the ceiling on ACTIVE processes — resolved from the Stripe product metadata of
-- the subscription's price; NULL until a plan is first known.

CREATE TABLE subscription (
  id                     uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id              uuid NOT NULL UNIQUE REFERENCES tenant(id),  -- 1 subscription per tenant
  stripe_customer_id     text UNIQUE,                                 -- Stripe Customer = tenant; UNIQUE indexes the lookup
  stripe_subscription_id text,
  status                 text NOT NULL,          -- trialing | active | past_due | canceled (validated in app)
  plan                   text,                   -- plan id/name from the Stripe product; NULL until first known
  current_period_end     timestamptz,            -- end of the paid-through window; NULL until a subscription.* sets it
  active_process_limit   int,                    -- v0 entitlement: ceiling on ACTIVE processes; from product metadata
  created_at             timestamptz NOT NULL DEFAULT now(),
  updated_at             timestamptz NOT NULL DEFAULT now()
);
-- No separate index on stripe_customer_id: the UNIQUE constraint already backs it
-- with a btree usable by FindByStripeCustomer.

-- Row-Level Security — barrier 2 of tenant isolation (docs/erd-backend.md §4d.4),
-- same molde as the tables in 0001 and 0005.
ALTER TABLE subscription ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON subscription
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

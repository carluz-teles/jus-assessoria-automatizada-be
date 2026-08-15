-- 0037_plan.up.sql — the local plan catalog (billing fatia 5, "BE é fonte de
-- verdade do catálogo"). Until now the plan/price ceiling was resolved live from
-- Stripe product metadata on every webhook (billing.ResolvePlan); this table makes
-- the BE the source of truth for WHAT the plans are (name, process-count bands,
-- price per process), Stripe only supplies the checkout price id per plan.
--
-- Reference data (NOT per-tenant, same molde as holiday/task_suggestion's peers):
-- every tenant reads the same catalog, so there is no tenant_id and NO RLS — a
-- global table, exactly like docs/erd-modelo-de-dados.md's other reference tables.
--
-- min_processes/max_processes are the [min,max] band the plan is priced for
-- (max_processes NULL = no ceiling, the Enterprise plan); price_per_process_cents
-- is the flat per-active-process price within that band. stripe_price_id bridges
-- to Stripe's Checkout price (NULL until a Stripe product is created for the plan
-- — out of scope for this fatia, see migration comment on subscription_plan_ref).

CREATE TABLE plan (
  id                       uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  code                     text NOT NULL UNIQUE,       -- starter | growth | scale | enterprise
  name                     text NOT NULL,
  min_processes            int  NOT NULL,
  max_processes            int,                        -- NULL = no ceiling (Enterprise)
  price_per_process_cents  int  NOT NULL,
  stripe_price_id          text UNIQUE,                -- NULL until the Stripe product/price exists
  active                   bool NOT NULL DEFAULT true,
  created_at               timestamptz NOT NULL DEFAULT now(),
  updated_at               timestamptz NOT NULL DEFAULT now()
);

-- Seed the v0 catalog (docs/fundacao-prd-erd.md pricing bands). stripe_price_id is
-- left NULL — wiring the Stripe products/prices for each plan is a follow-up
-- outside this migration's scope; FindPlanByStripePriceID simply finds no row
-- until that is done, which the webhook path surfaces as a typed error rather
-- than silently projecting an unknown plan.
INSERT INTO plan (code, name, min_processes, max_processes, price_per_process_cents) VALUES
  ('starter',    'Starter',    1,    100,  50),
  ('growth',     'Growth',     101,  500,  35),
  ('scale',      'Scale',      501,  1500, 20),
  ('enterprise', 'Enterprise', 1501, NULL, 15);

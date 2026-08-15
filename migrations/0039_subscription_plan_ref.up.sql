-- 0039_subscription_plan_ref.up.sql — links subscription to the local plan
-- catalog (migration 0037) and carries the per-tenant overrides billing fatia 5
-- needs: a negotiated custom price (Enterprise deals) and the trial window.
--
-- plan_id is nullable: existing subscription rows (and any projected before the
-- next webhook resolves a local plan) have no plan_id until the next
-- customer.subscription.* event re-runs applySubscription and populates it — this
-- migration does not backfill (there is no reliable price_id -> plan mapping for
-- historical rows without re-reading Stripe, out of scope here).

ALTER TABLE subscription
  ADD COLUMN plan_id uuid REFERENCES plan(id),
  ADD COLUMN custom_price_per_process_cents int,
  ADD COLUMN trial_ends_at timestamptz;

-- FK columns are not auto-indexed in Postgres; this backs FindPlanByCode/the
-- future join from subscription to its plan.
CREATE INDEX subscription_plan_id_idx ON subscription (plan_id);

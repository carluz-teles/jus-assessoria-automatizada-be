DROP INDEX subscription_plan_id_idx;

ALTER TABLE subscription
  DROP COLUMN plan_id,
  DROP COLUMN custom_price_per_process_cents,
  DROP COLUMN trial_ends_at;

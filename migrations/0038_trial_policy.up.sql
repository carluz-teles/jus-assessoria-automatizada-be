-- 0038_trial_policy.up.sql — the trial policy catalog (billing fatia 5). Trial
-- length and the active-process ceiling during a trial are today nowhere in the
-- BE; this table gives them a local home so a future trial use case (fatia 2 of
-- this migration effort — NOT implemented here) can read them without inventing a
-- constant. Reference data, same molde as plan: no tenant_id, no RLS.
--
-- Several named policies MAY exist (e.g. a promo cohort with a longer trial) but
-- exactly one must be the default a fresh signup gets; is_default + the partial
-- unique index below enforce that invariant in the database itself, not just in
-- application code.
--
-- trial_days=14 / active_process_limit=20 are a reasonable v0 starting point, NOT
-- a locked product decision — the actual numbers are pending product input and can
-- be changed with a follow-up data migration once decided.

CREATE TABLE trial_policy (
  id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name                 text NOT NULL,
  is_default           bool NOT NULL DEFAULT false,
  trial_days           int  NOT NULL,
  active_process_limit int  NOT NULL,
  active               bool NOT NULL DEFAULT true,
  created_at           timestamptz NOT NULL DEFAULT now()
);

-- At most one ACTIVE default policy at a time — a database-level invariant so a
-- misconfigured seed/admin write can never leave two candidates for "the" default
-- (FindDefaultTrialPolicy would then be ambiguous).
CREATE UNIQUE INDEX trial_policy_default_active_idx
  ON trial_policy (is_default)
  WHERE is_default AND active;

INSERT INTO trial_policy (name, is_default, trial_days, active_process_limit, active)
VALUES ('default', true, 14, 20, true);

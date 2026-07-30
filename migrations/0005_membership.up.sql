-- 0005_membership.up.sql — the join table for org membership (convites, fatia F2-1).
-- Faithful to docs/erd-modelo-de-dados.md (source of truth for the schema).
-- A membership is its OWN table (not columns on app_user): it is the ACTIVE/REMOVED
-- link between an app_user and its tenant, so removal/role-change (F2-2) mutate the
-- link without touching the user. status is text + CHECK-on-app (ACTIVE|REMOVED).

CREATE TABLE membership (
  id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id           uuid NOT NULL REFERENCES tenant(id),
  app_user_id         uuid NOT NULL REFERENCES app_user(id),
  clerk_membership_id text UNIQUE,                       -- bridge to Clerk; NULL for backfilled rows
  role                text NOT NULL,                     -- ADMIN | LAWYER (validated in app)
  status              text NOT NULL DEFAULT 'ACTIVE',    -- ACTIVE | REMOVED (validated in app)
  joined_at           timestamptz NOT NULL DEFAULT now(),
  removed_at          timestamptz,                       -- set on soft-delete (F2-2)
  UNIQUE (tenant_id, app_user_id)                        -- 1 membership per user per tenant
);
CREATE INDEX ON membership (tenant_id);

-- Backfill: every existing app_user is already an ACTIVE member of its tenant —
-- the org owner created during onboarding included. clerk_membership_id stays NULL
-- (we never saw a membership.created for these); the UNIQUE tolerates many NULLs.
INSERT INTO membership (tenant_id, app_user_id, role, status)
SELECT tenant_id, id, role, 'ACTIVE' FROM app_user;

-- Row-Level Security — barrier 2 of tenant isolation (docs/erd-backend.md §4d.4),
-- same molde as the tables in 0001.
ALTER TABLE membership ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON membership
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

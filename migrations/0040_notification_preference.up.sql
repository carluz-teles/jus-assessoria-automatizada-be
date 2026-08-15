-- 0037_notification_preference.up.sql — per-user, per-type channel opt-out
-- (docs/erd-sistemas-auxiliares.md §6). Absence of a row is the default: every
-- configured channel is enabled — the routing use case only consults this table to
-- find an EXPLICIT override, never to discover the default set. So a tenant with
-- nobody ever touching preferences behaves exactly as before this migration.
--
-- One row per (tenant, app_user, type): `channels` is the full enabled set for that
-- pair, not a delta — a PUT replaces it whole (ON CONFLICT DO UPDATE, see the sqlc
-- query), same idempotent-upsert molde as membership/tenant. channels is a bare
-- text[] (not a FK/enum table): the valid values are notification_delivery.channel's
-- own CHECK-on-app set (EMAIL, IN_APP, …), so adding a channel needs no migration
-- here either.

CREATE TABLE notification_preference (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id    uuid NOT NULL REFERENCES tenant(id),
  app_user_id  uuid NOT NULL REFERENCES app_user(id),
  type         text NOT NULL,
  channels     text[] NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, app_user_id, type)
);
CREATE INDEX ON notification_preference (tenant_id);

-- Row-Level Security — barrier 2 of tenant isolation, same molde as notification /
-- notification_delivery / notification_read (0008/0018).
ALTER TABLE notification_preference ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON notification_preference
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

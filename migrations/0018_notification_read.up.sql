-- 0018_notification_read.up.sql — per-USER read state for the in-app inbox.
--
-- Slice 1a (0017) added notification.read_at as a single tenant-wide flag, but the
-- read state is per USER: two members of the same escritório read the same aviso
-- independently. This table records one receipt per (aviso, user); its ABSENCE is
-- "unread" for that user, so a fresh aviso is unread for everyone with no backfill.
-- The old tenant-wide column (dormant — slice 1a only ever wrote it NULL) is dropped
-- in the same step, so there is one source of truth for read state.

CREATE TABLE notification_read (
  notification_id uuid NOT NULL REFERENCES notification(id),
  user_id         uuid NOT NULL REFERENCES app_user(id),
  tenant_id       uuid NOT NULL REFERENCES tenant(id),
  read_at         timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (notification_id, user_id)   -- one receipt per aviso per user (idempotent)
);
CREATE INDEX ON notification_read (tenant_id);

-- Row-Level Security — barrier 2 of tenant isolation, same molde as notification /
-- notification_delivery (0008).
ALTER TABLE notification_read ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON notification_read
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- Drop the tenant-wide read flag: read state is per-user now (the table above).
ALTER TABLE notification DROP COLUMN read_at;

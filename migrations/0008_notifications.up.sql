-- 0008_notifications.up.sql — the avisos domain (internal/notifications). A
-- `notification` is a user-facing aviso (distinct from an INTIMAÇÃO, which 0006
-- renamed to `intimation` to free this name). The domain reacts to the generic
-- `notification.requested` event and delivers by channel; v0 ships EMAIL only.
--
-- Two tables, one aggregate: `notification` is the fact ("this user should be
-- told X"); `notification_delivery` is the per-channel attempt to deliver it, so
-- a future SMS/IN_APP channel is another delivery row, not another notification.
-- UNIQUE(notification_id, channel) makes each channel deliverable at most once
-- per notification (the idempotency floor beneath the consumer-side event dedup).
--
-- status columns are text + CHECK-on-app (the same convention as the other enums):
--   notification.status          — CREATED
--   notification_delivery.status — QUEUED | SENT | FAILED | BOUNCED

CREATE TABLE notification (
  id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id         uuid NOT NULL REFERENCES tenant(id),
  recipient_user_id uuid REFERENCES app_user(id),   -- nullable: some avisos are tenant-level
  type              text NOT NULL,                   -- template selector, e.g. member_joined
  payload           jsonb NOT NULL DEFAULT '{}',     -- the template data
  status            text NOT NULL DEFAULT 'CREATED', -- CREATED (validated in app)
  created_at        timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ON notification (tenant_id);

CREATE TABLE notification_delivery (
  id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  notification_id     uuid NOT NULL REFERENCES notification(id),
  tenant_id           uuid NOT NULL REFERENCES tenant(id),
  channel             text NOT NULL,                   -- EMAIL (validated in app)
  status              text NOT NULL DEFAULT 'QUEUED',  -- QUEUED | SENT | FAILED | BOUNCED
  provider_message_id text,                            -- the provider's id once SENT
  error               text,                            -- the failure reason once FAILED
  created_at          timestamptz NOT NULL DEFAULT now(),
  updated_at          timestamptz NOT NULL DEFAULT now(),
  UNIQUE (notification_id, channel)                    -- one delivery per channel per notification
);
CREATE INDEX ON notification_delivery (tenant_id);

-- Row-Level Security — barrier 2 of tenant isolation (docs/erd-backend.md §4d.4),
-- same molde as the tables in 0001/0005/0007.
ALTER TABLE notification ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON notification
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

ALTER TABLE notification_delivery ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON notification_delivery
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

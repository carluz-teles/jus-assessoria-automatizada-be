ALTER TABLE notification
  ADD COLUMN severidade      text NOT NULL DEFAULT 'info'
    CHECK (severidade IN ('info', 'atencao', 'critico')),
  ADD COLUMN group_key       text,
  ADD COLUMN expires_at      timestamptz,
  ADD COLUMN source_kind     text,
  ADD COLUMN source_id       uuid,
  ADD COLUMN source_event_id text,
  ADD COLUMN court_case_id   uuid;

CREATE UNIQUE INDEX notification_source_event_id_key ON notification (source_event_id)
  WHERE source_event_id IS NOT NULL;

CREATE INDEX ON notification (tenant_id, court_case_id);

ALTER TABLE notification_delivery
  ADD COLUMN motivo      text,
  ADD COLUMN seen_at     timestamptz,
  ADD COLUMN archived_at timestamptz;

CREATE TABLE seen_marker (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id    uuid NOT NULL REFERENCES tenant(id),
  app_user_id  uuid NOT NULL REFERENCES app_user(id),
  escopo_kind  text NOT NULL,
  escopo_id    uuid NOT NULL,
  last_seen_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, app_user_id, escopo_kind, escopo_id)
);

ALTER TABLE seen_marker ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON seen_marker
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

CREATE TABLE process_activity_log (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id       uuid NOT NULL REFERENCES tenant(id),
  court_record_id uuid NOT NULL REFERENCES court_record(id),

  event_type      text NOT NULL CHECK (event_type IN (
                     'INTIMATION_ANALYSIS_COMPLETED',
                     'DRAFT_GENERATED'
                   )),
  payload         jsonb NOT NULL DEFAULT '{}',

  occurred_at     timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX ON process_activity_log (court_record_id, occurred_at DESC, id DESC);

ALTER TABLE process_activity_log ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON process_activity_log
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

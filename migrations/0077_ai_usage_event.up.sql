CREATE TABLE ai_usage_event (
  id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id         uuid NOT NULL REFERENCES tenant(id),
  use_case          text NOT NULL,
  provider          text NOT NULL DEFAULT 'openrouter',
  model             text NOT NULL,
  prompt_tokens     integer NOT NULL DEFAULT 0,
  completion_tokens integer NOT NULL DEFAULT 0,
  total_tokens      integer NOT NULL DEFAULT 0,
  cached_tokens     integer NOT NULL DEFAULT 0,
  cost_usd          numeric(12, 6) NOT NULL DEFAULT 0,
  latency_ms        integer NOT NULL DEFAULT 0,
  trace_id          text,
  created_at        timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ON ai_usage_event (tenant_id, use_case, created_at);

ALTER TABLE ai_usage_event ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON ai_usage_event
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

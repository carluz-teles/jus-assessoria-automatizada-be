CREATE TABLE onboarding_widget_dismissal (
  app_user_id  uuid PRIMARY KEY REFERENCES app_user(id),
  tenant_id    uuid NOT NULL REFERENCES tenant(id),
  dismissed_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ON onboarding_widget_dismissal (tenant_id);

ALTER TABLE onboarding_widget_dismissal ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON onboarding_widget_dismissal
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

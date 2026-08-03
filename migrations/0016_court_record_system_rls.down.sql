-- Reverso da 0016 — volta a policy de court_record ao isolamento só-por-tenant.
ALTER POLICY tenant_isolation ON court_record
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

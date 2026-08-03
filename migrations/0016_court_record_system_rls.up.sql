-- 0016_court_record_system_rls — the re-poll scheduler must scan court_record
-- across ALL tenants (next_sync_at is the hottest query), but court_record is
-- RLS-isolated by tenant_id, so a system process reading with no app.tenant_id
-- sees nothing. Widen the tenant_isolation policy with a SYSTEM escape hatch: a
-- transaction that sets app.system='on' (the scheduler pool) reads every row; a
-- normal request (app.tenant_id set, app.system unset) is unchanged — the OR's
-- second arm is NULL/false, so isolation holds. A transaction with NEITHER set
-- still sees nothing (fail-closed).
ALTER POLICY tenant_isolation ON court_record
  USING (
    tenant_id = current_setting('app.tenant_id', true)::uuid
    OR current_setting('app.system', true) = 'on'
  );

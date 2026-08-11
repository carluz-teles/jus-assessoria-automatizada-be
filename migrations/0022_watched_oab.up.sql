-- watched_oab denormalizes each integration's watched OABs into one row per
-- (integration, OAB), keyed by the SAME normalized "NUMBER|UF" key the publication
-- store indexes in recipient_oabs. It is the join that turns the national firehose
-- into per-tenant intimations: the local match looks up which tenants watch a
-- communication's recipient OABs. Populated from the integration_activated event;
-- the source of truth stays integration.scope.
CREATE TABLE watched_oab (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      uuid NOT NULL REFERENCES tenant(id),
    integration_id uuid NOT NULL REFERENCES integration(id) ON DELETE CASCADE,
    oab_key        text NOT NULL, -- normalized "NUMBER|UF"; matches publication.recipient_oabs
    created_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (integration_id, oab_key)
);

-- The match lookup: publication.recipient_oabs (a set) ANY-joins watched_oab.oab_key.
CREATE INDEX watched_oab_oab_key_idx ON watched_oab (oab_key);
-- FK index (Postgres does not auto-create it): the populate delete + cascade filter here.
CREATE INDEX watched_oab_integration_id_idx ON watched_oab (integration_id);

-- Tenant isolation with the system escape hatch (mirrors court_record's 0016 policy):
-- populate runs per-tenant (app.tenant_id set); the national match reads across tenants
-- via uow.DoSystem (app.system='on'). A tx with neither set sees nothing (fail-closed).
ALTER TABLE watched_oab ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON watched_oab
  USING (
    tenant_id = current_setting('app.tenant_id', true)::uuid
    OR current_setting('app.system', true) = 'on'
  );

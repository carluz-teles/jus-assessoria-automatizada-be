-- watched_oab queries (acquisition slice). The per-tenant index of watched OABs the
-- national match joins against. Populated from the integration_activated event: the
-- use case replaces an integration's set (delete + insert) so a scope change is
-- reflected. Writes run per-tenant (RLS by app.tenant_id); the match reads system-wide.

-- name: DeleteWatchedOABsByIntegration :exec
-- Clear an integration's watched OABs before re-populating, so a removed OAB stops
-- matching. Scoped to the integration (the tenant is implied by the RLS tx).
DELETE FROM watched_oab WHERE integration_id = $1;

-- name: InsertWatchedOAB :exec
-- Add one watched OAB for an integration, idempotent on (integration_id, oab_key):
-- a re-activation with the same scope is a no-op. oab_key is the normalized "NUMBER|UF".
INSERT INTO watched_oab (tenant_id, integration_id, oab_key)
VALUES ($1, $2, $3)
ON CONFLICT (integration_id, oab_key) DO NOTHING;

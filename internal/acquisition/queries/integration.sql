-- acquisition slice queries (integration).
-- The upsert is keyed by (tenant_id, source) so re-activating a source is
-- idempotent at the row level; credential_ref is never written from here (it
-- stays NULL in v0) and is preserved across updates.

-- name: UpsertIntegration :one
-- Activate (or re-activate) a tenant's subscription to a data source. On
-- conflict only scope/status/updated_at change — credential_ref is left intact.
INSERT INTO integration (tenant_id, source, scope, status)
VALUES ($1, $2, $3, 'ACTIVE')
ON CONFLICT (tenant_id, source) DO UPDATE
   SET scope      = EXCLUDED.scope,
       status     = 'ACTIVE',
       updated_at = now()
RETURNING *;

-- name: GetIntegrationBySource :one
SELECT * FROM integration
WHERE tenant_id = $1 AND source = $2;

-- name: ListIntegrations :many
-- All of a tenant's integrations, oldest first. tenant_id filter is isolation
-- barrier 1 (the app layer); RLS is barrier 2.
SELECT * FROM integration
WHERE tenant_id = $1
ORDER BY created_at, id;

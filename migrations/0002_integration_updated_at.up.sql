-- 0002_integration_updated_at.up.sql — add integration.updated_at.
-- The acquisition slice touches an integration row on every re-activation
-- (upsert on (tenant_id, source)); updated_at records the last activation so a
-- reactivation is observable even when scope is unchanged. NOT NULL DEFAULT now()
-- backfills existing rows with the migration timestamp.

ALTER TABLE integration ADD COLUMN updated_at timestamptz NOT NULL DEFAULT now();

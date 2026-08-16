-- 0042_portal_credential.down.sql — revert, child table first (FK to tenant_secret).

DROP TABLE IF EXISTS portal_credential;
DROP TABLE IF EXISTS tenant_secret;

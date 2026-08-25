DROP INDEX IF EXISTS draft_tenant_created_by_idx;
ALTER TABLE draft DROP COLUMN IF EXISTS created_by;

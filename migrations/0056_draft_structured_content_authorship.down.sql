DROP INDEX IF EXISTS draft_tenant_authorship_idx;

ALTER TABLE draft
    DROP CONSTRAINT IF EXISTS draft_authorship_check;

ALTER TABLE draft
    DROP COLUMN IF EXISTS authorship,
    DROP COLUMN IF EXISTS structured_content;

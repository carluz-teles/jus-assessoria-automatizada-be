-- 0030 down — drop the triagem index then the column. The DJEN cancellation
-- `status` column (0014) is untouched.
DROP INDEX IF EXISTS intimation_tenant_user_status_idx;
ALTER TABLE intimation
    DROP COLUMN user_status;

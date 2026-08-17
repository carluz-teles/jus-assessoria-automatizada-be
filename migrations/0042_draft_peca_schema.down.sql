-- 0042_draft_peca_schema.down — reverts the peticionamento Fatia 1 schema changes.
-- Drops the new indexes first (dependencies before columns), then removes the columns
-- and restores storage_key NOT NULL (the 0001 baseline state).

DROP INDEX IF EXISTS draft_intimation_id_uidx;
DROP INDEX IF EXISTS draft_intimation_id_idx;

ALTER TABLE draft
    DROP COLUMN IF EXISTS intimation_id,
    DROP COLUMN IF EXISTS title,
    DROP COLUMN IF EXISTS content,
    DROP COLUMN IF EXISTS updated_at,
    ALTER COLUMN storage_key SET NOT NULL;

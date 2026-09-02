-- Reverte 0093. Remove primeiro as linhas intimation-scoped (não têm draft_id, então
-- violariam o NOT NULL restaurado), depois derruba index/constraint/coluna e volta o
-- draft_id a NOT NULL.
DELETE FROM suggested_thesis WHERE draft_id IS NULL;

DROP INDEX IF EXISTS suggested_thesis_intimation_id_idx;

ALTER TABLE suggested_thesis
    DROP CONSTRAINT IF EXISTS suggested_thesis_scope_chk;

ALTER TABLE suggested_thesis
    DROP COLUMN IF EXISTS intimation_id;

ALTER TABLE suggested_thesis
    ALTER COLUMN draft_id SET NOT NULL;

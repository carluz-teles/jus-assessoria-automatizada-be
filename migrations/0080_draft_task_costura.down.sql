-- Revert 0080: restore the ORIGINAL (0042) unique index exactly, then drop the
-- columns it and draft_task_id_uidx depend on. draft_task_id_uidx and the plain
-- indexes on task_id/piece_profile_key/superseded_by_draft_id are also implicitly
-- dropped by DROP COLUMN, but dropped explicitly first for clarity/order.
--
-- CAVEAT — this rollback is ONE-WAY once the feature has been used for real: once
-- >=2 task-sourced drafts share the same intimation_id (the exact N-providências
-- scenario 0080 exists to allow — docs/erd-costura-providencia-tarefa-peca.md §1),
-- the CREATE UNIQUE INDEX below fails with 23505 (duplicate tenant_id+intimation_id)
-- and golang-migrate is left DIRTY (no data is lost — the DROP COLUMNs above roll
-- back inside the same failed transaction — but the schema_migrations row must be
-- fixed by hand). Recovery: decide which duplicate draft(s) to consolidate/delete
-- per intimation_id, then `migrate force 79` before retrying. Do not roll back 0080
-- in an environment where task-sourced drafts may already exist without checking
-- for this first.
DROP INDEX IF EXISTS draft_task_id_uidx;
DROP INDEX IF EXISTS draft_intimation_id_uidx;

ALTER TABLE draft
    DROP COLUMN IF EXISTS superseded_by_draft_id,
    DROP COLUMN IF EXISTS superseded_at,
    DROP COLUMN IF EXISTS piece_profile_key,
    DROP COLUMN IF EXISTS task_id;

CREATE UNIQUE INDEX draft_intimation_id_uidx
    ON draft (tenant_id, intimation_id)
    WHERE intimation_id IS NOT NULL;

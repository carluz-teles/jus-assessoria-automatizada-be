-- Revert 0089: restore the 0088 constraint exactly (task_id IS NOT NULL, no
-- superseded_at scoping).
--
-- CAVEAT — same one-way rollback risk 0088's down already documents, one level deeper:
-- once a SINGLE providência has actually been reclassified after its peça existed (>=2
-- draft rows — one superseded, one vigente — sharing the same tenant_id+task_id), the
-- CREATE UNIQUE INDEX below fails with 23505 and golang-migrate is left DIRTY. No data is
-- lost (this file only touches an index), but the schema_migrations row must be fixed by
-- hand. Recovery: decide which draft(s) to consolidate/delete per task_id (or hard-delete
-- the superseded ones, if the history is not worth keeping), then `migrate force 88` before
-- retrying. Do not roll back 0089 in an environment where the reclassify flow may already
-- have produced a superseded+vigente pair without checking for this first.
DROP INDEX IF EXISTS draft_task_id_uidx;
CREATE UNIQUE INDEX draft_task_id_uidx
    ON draft (tenant_id, task_id)
    WHERE task_id IS NOT NULL;

-- 0081: restrict draft_task_id_uidx to the VIGENTE draft per task (fatia 5, docs/erd-
-- costura-providencia-tarefa-peca.md §7 questão 4 — "reclassificação depois de gerada a
-- peça" — the Architect's decision: descartar e recomeçar, never mutate the peça in place).
--
-- 0080's draft_task_id_uidx enforces "at most one draft per (tenant, task_id)" — correct
-- until a providência can be reclassified AFTER its peça exists. When that happens, the OLD
-- draft is superseded (superseded_at = now(), never deleted — internal/draft's
-- SupersedeDraftForTask) and a fresh POST /v1/pecas {task_id} must be able to mint the
-- corrected draft for the SAME task_id. Scoping the unique index to
-- `superseded_at IS NULL` lets exactly one row be "vigente" per task at a time while the
-- superseded history stays queryable (superseded_by_draft_id then links old → new).
DROP INDEX draft_task_id_uidx;
CREATE UNIQUE INDEX draft_task_id_uidx
    ON draft (tenant_id, task_id)
    WHERE task_id IS NOT NULL AND superseded_at IS NULL;

-- 0080: draft.task_id + draft.piece_profile_key (docs/erd-costura-providencia-tarefa-
-- peca.md §2/§3) — the task-sourced Create flow (POST /v1/pecas with a task_id body
-- field) inherits its piece_type from the providência (action_item) the task stems
-- from, never re-choosing it (§3, "a Minuta herda, não re-escolhe"). Also adds
-- superseded_at/superseded_by_draft_id (§7 questão 4, reclassificação pós-peça) as
-- SCHEMA ONLY in this fatia — no application code reads/writes them yet.
--
-- Column choices, matched to the repo's existing conventions:
--   * task_id references task(id) — nullable: only Create's task-sourced path sets
--     it; every existing draft (source=intimation/processo/blank) leaves it NULL.
--   * piece_profile_key references piece_profile(key) — nullable, mirrors
--     action_item.piece_profile_key (0078): a draft always has a piece_type (text,
--     free-form label), but only a task-sourced draft also carries the STRUCTURED
--     catalog key it was derived from.
--   * superseded_at/superseded_by_draft_id: nullable, self-referential FK. Future
--     fatia writes them when an advogado reclassifies a peça already generated.
ALTER TABLE draft
    ADD COLUMN task_id                uuid REFERENCES task(id),
    ADD COLUMN piece_profile_key      text REFERENCES piece_profile(key),
    ADD COLUMN superseded_at          timestamptz,
    ADD COLUMN superseded_by_draft_id uuid REFERENCES draft(id);

-- FK columns need their own index (Postgres does not auto-index them).
CREATE INDEX ON draft (task_id);
CREATE INDEX ON draft (piece_profile_key);
CREATE INDEX ON draft (superseded_by_draft_id);

-- Restrict the LEGACY idempotency guard (0042) to the non-task path. Without this,
-- the ERD §1 central example (1 intimação → N providências → N distinct peças) would
-- collide with it: two task-sourced drafts for the SAME intimation_id (one per task)
-- must both be allowed. GetDraftByIntimationID (the legacy idempotent-fetch path)
-- still relies on this being unique for task_id IS NULL rows.
DROP INDEX draft_intimation_id_uidx;
CREATE UNIQUE INDEX draft_intimation_id_uidx
    ON draft (tenant_id, intimation_id)
    WHERE intimation_id IS NOT NULL AND task_id IS NULL;

-- NEW idempotency guard for the task-sourced path: at most one draft per (tenant,
-- task) — a redelivered/duplicate POST /v1/pecas {task_id} never mints a 2nd draft.
-- Distinct tasks (even from the same intimação) are unrestricted between each other.
CREATE UNIQUE INDEX draft_task_id_uidx
    ON draft (tenant_id, task_id)
    WHERE task_id IS NOT NULL;

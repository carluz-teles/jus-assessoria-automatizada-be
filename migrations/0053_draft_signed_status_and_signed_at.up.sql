-- Fatia 4 — peticionamento: add SIGNED status and signed_at column.
--
-- Status lifecycle: DRAFT → REVIEWED → SIGNED (milestone lifecycle, distinct
-- from saga_state which tracks the async AI pipeline).
--
-- signed_at records when the peça was digitally signed; nullable (NULL until
-- the sign endpoint is called).

ALTER TABLE draft
    ADD COLUMN signed_at timestamptz;

-- Replace the status CHECK to include SIGNED. The init migration had no CHECK
-- on draft.status, so we add one now to enforce the closed set at the DB level.
ALTER TABLE draft
    ADD CONSTRAINT draft_status_check
    CHECK (status IN ('DRAFT', 'REVIEWED', 'SIGNED'));

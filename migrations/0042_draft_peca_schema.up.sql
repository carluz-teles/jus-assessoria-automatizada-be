-- 0042_draft_peca_schema — evolves the draft table for the peticionamento Fatia 1
-- (POST /v1/pecas, GET /v1/pecas/:id, PATCH /v1/pecas/:id).
--
-- Changes from the 0001 baseline:
--   1. intimation_id — nullable FK to intimation(id); the unique index below enforces
--      at most one draft per (tenant_id, intimation_id) (the idempotency guarantee).
--   2. title — the peça title; NOT NULL DEFAULT '' so existing rows migrate cleanly.
--   3. content — the peça body, stored in the column (not in S3); nullable (initially
--      empty when the editor opens).
--   4. updated_at — autosave timestamp; NOT NULL DEFAULT now() for existing rows.
--   5. storage_key drops its NOT NULL constraint — it was required in 0001 but the
--      peticionamento design moved content to the column (F1); F4 (protocolo) may
--      re-introduce a key, but the column must be nullable here.
--
-- The RLS policy on draft already covers the new columns; no new policy needed.

ALTER TABLE draft
    ADD COLUMN intimation_id uuid REFERENCES intimation(id),
    ADD COLUMN title         text NOT NULL DEFAULT '',
    ADD COLUMN content       text,
    ADD COLUMN updated_at    timestamptz NOT NULL DEFAULT now(),
    ALTER COLUMN storage_key DROP NOT NULL;

-- Unique constraint: only ONE draft per tenant per intimation (idempotent POST).
-- Partial (WHERE intimation_id IS NOT NULL) so blank/processo drafts are unrestricted.
CREATE UNIQUE INDEX draft_intimation_id_uidx
    ON draft (tenant_id, intimation_id)
    WHERE intimation_id IS NOT NULL;

-- Plain index for JOIN/lookup by intimation_id (the GET read model joins draft+intimation).
CREATE INDEX draft_intimation_id_idx ON draft (intimation_id);

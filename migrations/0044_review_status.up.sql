-- 0044_review_status — adds lifecycle columns to the review table so the AI
-- generation saga can represent COMPLETED (success) and FAILED (LLM error or
-- nil-generator) outcomes, and the read model can serve the latest review per draft
-- efficiently.
--
-- review has NO tenant_id — isolation is via JOIN draft.tenant_id (barrier 1). RLS
-- is therefore absent on this table today. This is acceptable because:
--   (a) every API access goes through GetDraftDetail (draft.tenant_id guard) or the
--       generate handler (GetDraftByID tenant guard), never direct review reads.
--   (b) Adding an RLS policy on review requires either duplicating tenant_id here or
--       a policy that checks CURRENT draft ownership, which adds query complexity.
-- RECOMMENDATION: add tenant_id NOT NULL + RLS on a future slice (see KNOWN_RISKS in
-- the fatia-3 implementation summary). For now barrier-1 is the enforced guard.

ALTER TABLE review
    ADD COLUMN status       text NOT NULL DEFAULT 'COMPLETED'
                                CHECK (status IN ('COMPLETED', 'FAILED')),
    ADD COLUMN generated_at timestamptz NOT NULL DEFAULT now();

-- Supports the read model query: latest review for a draft (draft_id + generated_at DESC).
-- The name includes the constraint semantics so future migrations can identify it.
CREATE INDEX review_draft_latest_idx ON review (draft_id, generated_at DESC);

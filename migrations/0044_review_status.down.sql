-- Revert 0044_review_status.
DROP INDEX IF EXISTS review_draft_latest_idx;

ALTER TABLE review
    DROP COLUMN IF EXISTS status,
    DROP COLUMN IF EXISTS generated_at;

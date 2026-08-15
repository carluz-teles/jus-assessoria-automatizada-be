-- Reverts 0036_deadline_ai_summary.
ALTER TABLE deadline
    DROP COLUMN IF EXISTS ai_summary_generated_at,
    DROP COLUMN IF EXISTS ai_recommendation,
    DROP COLUMN IF EXISTS ai_summary;

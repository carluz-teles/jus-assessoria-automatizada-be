-- 0040_court_record_ai_resume.down.sql — revert the AI resume columns.

ALTER TABLE court_record
    DROP COLUMN IF EXISTS ai_resume,
    DROP COLUMN IF EXISTS ai_resume_generated_at;
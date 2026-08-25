-- Rollback: drop constraints before columns (constraints reference the columns).

ALTER TABLE draft DROP CONSTRAINT IF EXISTS draft_instructions_length_check;
ALTER TABLE draft DROP CONSTRAINT IF EXISTS draft_tone_check;

ALTER TABLE draft DROP COLUMN IF EXISTS selected_theses;
ALTER TABLE draft DROP COLUMN IF EXISTS instructions;
ALTER TABLE draft DROP COLUMN IF EXISTS tone;

-- Rollback Fatia 4 — peticionamento status and signed_at.

ALTER TABLE draft DROP CONSTRAINT IF EXISTS draft_status_check;
ALTER TABLE draft DROP COLUMN IF EXISTS signed_at;

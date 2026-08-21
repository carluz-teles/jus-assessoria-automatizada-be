-- Revert 0046_capture_run.
DROP INDEX IF EXISTS deadline_tenant_created_idx;
ALTER TABLE deadline DROP COLUMN IF EXISTS created_at;
ALTER TABLE sync_run DROP COLUMN IF EXISTS court_records_updated;
DROP TABLE IF EXISTS capture_run;

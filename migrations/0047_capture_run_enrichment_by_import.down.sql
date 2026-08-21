-- Revert 0047_capture_run_enrichment_by_import.
DROP INDEX IF EXISTS capture_run_enrichment_import_uq;
DROP INDEX IF EXISTS capture_run_backfill_job_idx;
ALTER TABLE capture_run DROP COLUMN IF EXISTS backfill_job_id;

DROP INDEX IF EXISTS sync_run_backfill_job_id_idx;
DROP INDEX IF EXISTS intimation_sync_run_id_idx;
DROP INDEX IF EXISTS court_record_sync_run_id_idx;

ALTER TABLE sync_run
  DROP COLUMN backfill_job_id,
  DROP COLUMN court_records_new,
  DROP COLUMN intimations_new;

ALTER TABLE intimation   DROP COLUMN sync_run_id;
ALTER TABLE court_record DROP COLUMN sync_run_id;

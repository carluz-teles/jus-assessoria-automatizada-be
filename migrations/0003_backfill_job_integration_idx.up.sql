-- 0003_backfill_job_integration_idx.up.sql — index backfill_job(integration_id).
-- The backfill listener's first-activation guard (BackfillJobExistsByIntegration)
-- queries backfill_job by integration_id on every integration_activated event;
-- without this index that guard degrades to a seq scan as the table grows.

CREATE INDEX idx_backfill_job_integration_id ON backfill_job (integration_id);

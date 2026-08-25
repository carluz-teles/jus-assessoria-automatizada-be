ALTER TABLE watched_oab DROP COLUMN last_action, DROP COLUMN last_action_at;
ALTER TABLE sync_run DROP COLUMN trigger_reason, DROP COLUMN trigger_oab;
ALTER TABLE backfill_job DROP COLUMN trigger_reason, DROP COLUMN trigger_oabs;

-- 0019_sync_run_window.down.sql — drop the sync_run window bounds.
ALTER TABLE sync_run
  DROP COLUMN window_from,
  DROP COLUMN window_to;

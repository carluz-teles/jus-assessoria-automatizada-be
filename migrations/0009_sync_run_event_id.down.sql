-- 0009_sync_run_event_id.down.sql — reverses 0009. The index would drop with the
-- column, but drop it explicitly first for symmetry with the up migration.

DROP INDEX IF EXISTS sync_run_event_id_key;
ALTER TABLE sync_run DROP COLUMN IF EXISTS event_id;

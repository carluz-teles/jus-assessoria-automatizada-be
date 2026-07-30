-- 0009_sync_run_event_id.up.sql — track the sync_requested event that opened a
-- sync_run, so a re-delivery of an event whose run never closed (an infra fault
-- between the dedup mark in UoW-1 and the OK/FAILED close) can RESUME and close
-- that same run instead of no-op'ing it into a permanent RUNNING.
--
-- Nullable on purpose: rows written before this migration keep NULL, and a PARTIAL
-- unique index (WHERE event_id IS NOT NULL) forbids two runs sharing an event_id
-- while still permitting many NULLs (legacy rows) — a plain UNIQUE would reject the
-- second NULL.

ALTER TABLE sync_run ADD COLUMN event_id text;
CREATE UNIQUE INDEX sync_run_event_id_key ON sync_run (event_id) WHERE event_id IS NOT NULL;

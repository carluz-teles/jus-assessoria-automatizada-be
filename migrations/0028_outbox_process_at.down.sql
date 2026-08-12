-- 0028_outbox_process_at.down.sql — revert opt-in future delivery.
ALTER TABLE outbox DROP COLUMN process_at;

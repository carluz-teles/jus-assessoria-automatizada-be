-- 0018_notification_read.down.sql — restore the tenant-wide read flag (0017) and
-- drop the per-user receipts table.
ALTER TABLE notification ADD COLUMN read_at timestamptz;

DROP TABLE notification_read;

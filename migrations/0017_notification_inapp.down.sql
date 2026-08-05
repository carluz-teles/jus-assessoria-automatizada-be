-- 0017_notification_inapp.down.sql — drop the in-app channel fields.
ALTER TABLE notification
  DROP COLUMN title,
  DROP COLUMN body,
  DROP COLUMN read_at;

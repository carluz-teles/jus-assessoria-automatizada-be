-- 0017_notification_inapp.up.sql — in-app channel fields on the notification aviso.
--
-- The IN_APP channel (slice 1a) differs from EMAIL on two points that need columns:
-- it MATERIALIZES the title/body at write time (EMAIL renders them in the channel at
-- send), and it tracks a per-aviso READ state for the in-app inbox. All three columns
-- are nullable and NULL for the existing EMAIL avisos, which materialize nothing here.
ALTER TABLE notification
  ADD COLUMN title   text,
  ADD COLUMN body    text,
  ADD COLUMN read_at timestamptz;

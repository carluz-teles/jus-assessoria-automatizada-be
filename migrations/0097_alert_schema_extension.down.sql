DROP TABLE seen_marker;

ALTER TABLE notification_delivery
  DROP COLUMN archived_at,
  DROP COLUMN seen_at,
  DROP COLUMN motivo;

ALTER TABLE notification
  DROP COLUMN court_case_id,
  DROP COLUMN source_event_id,
  DROP COLUMN source_id,
  DROP COLUMN source_kind,
  DROP COLUMN expires_at,
  DROP COLUMN group_key,
  DROP COLUMN severidade;

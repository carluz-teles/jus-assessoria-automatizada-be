-- 0002_integration_updated_at.down.sql — reverses 0002_integration_updated_at.up.sql.

ALTER TABLE integration DROP COLUMN updated_at;

DROP INDEX IF EXISTS watched_oab_enabled_idx;

ALTER TABLE watched_oab
  DROP COLUMN enabled,
  DROP COLUMN disabled_at,
  DROP COLUMN catch_up_since;

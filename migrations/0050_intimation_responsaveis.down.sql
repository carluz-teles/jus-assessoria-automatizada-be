-- Reverte 0050: remove responsáveis da intimação. Destrutivo: os vínculos são perdidos.
DROP INDEX IF EXISTS intimation_conductor_user_id_idx;
DROP INDEX IF EXISTS intimation_reviewer_user_id_idx;

ALTER TABLE intimation
  DROP COLUMN conductor_user_id,
  DROP COLUMN reviewer_user_id;

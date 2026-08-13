-- 0029 down — drop the responsável column. The FK and its ON DELETE SET NULL go with it.
ALTER TABLE court_case
  DROP COLUMN assigned_user_id;

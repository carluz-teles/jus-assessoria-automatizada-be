-- 0053 down — drop the task priority column (the CHECK goes with it).
ALTER TABLE task DROP COLUMN IF EXISTS priority;

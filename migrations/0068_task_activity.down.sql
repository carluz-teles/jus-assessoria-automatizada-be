-- 0055 down — drop the task audit log (the RLS policy and the index go with the table).
DROP TABLE IF EXISTS task_activity;

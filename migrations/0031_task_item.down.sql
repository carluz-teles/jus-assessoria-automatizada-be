-- 0031 down — drop the checklist table. The RLS policy and the index go with the table
-- (DROP TABLE cascades both); the CASCADE FK to task drops no task rows (it only governs
-- deletes of task, not of task_item).
DROP TABLE IF EXISTS task_item;

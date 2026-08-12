-- Revert 0025: drop the deadline/task indexes it added. IF EXISTS so a partial
-- application rolls back cleanly.
DROP INDEX IF EXISTS task_assignee_user_id_idx;
DROP INDEX IF EXISTS task_intimation_id_idx;
DROP INDEX IF EXISTS task_deadline_id_idx;
DROP INDEX IF EXISTS task_court_record_id_idx;
DROP INDEX IF EXISTS deadline_tenant_id_idx;

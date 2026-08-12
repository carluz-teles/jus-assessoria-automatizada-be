-- 0025_deadline_indexes — the indexing advisories from the 0024 (2b) review.
-- 0024 shaped the prazos schema (deadline deltas + deadline_rule + task) but shipped
-- no supporting indexes beyond what 0001 already had. Two gaps to close:
--   1. deadline.tenant_id — the RLS policy filters every deadline read by tenant_id
--      (current_setting('app.tenant_id')), and the /prazos agenda lists by tenant; a
--      plain b-tree on tenant_id keeps both off a seq scan as the table grows.
--   2. task's FKs — an unindexed FK makes the referenced row's delete/update scan the
--      child, and the agenda joins task back to its court_record/deadline/intimation and
--      filters by assignee. Index each FK the slice will actually traverse.
-- task already has (tenant_id, status) and a partial (due_date) index from 0024; these
-- add the FK-side coverage. Index-only migration — no data change, safe to run anytime.

-- deadline: the RLS/agenda filter column.
CREATE INDEX IF NOT EXISTS deadline_tenant_id_idx ON deadline (tenant_id);

-- task: the four FKs the agenda and referential integrity traverse.
CREATE INDEX IF NOT EXISTS task_court_record_id_idx  ON task (court_record_id);
CREATE INDEX IF NOT EXISTS task_deadline_id_idx      ON task (deadline_id);
CREATE INDEX IF NOT EXISTS task_intimation_id_idx    ON task (intimation_id);
CREATE INDEX IF NOT EXISTS task_assignee_user_id_idx ON task (assignee_user_id);

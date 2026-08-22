-- 0055_task_activity — the audit log of a task (docs/erd-prazos.md §4/§10, the Tarefa detail's
-- "Atividade" tab). Every meaningful mutation of a task appends ONE row here, IN THE SAME TX as
-- the mutation itself (transactional — the log never diverges from the task's real history):
-- create, each edited field (title/description/due_date/assignee/priority), done, dismiss, and
-- comment. The detail view lists them newest-first. from_value/to_value carry the "de X para Y"
-- an edit renders; a create/done/dismiss/comment leaves them NULL.
--
-- SCHEMA ONLY — zero slice logic (that is internal/deadline). task has rows already; task_activity
-- starts empty (rows are written only by the new mutation paths going forward), so the NOT NULL
-- columns are safe (historical tasks simply have no log until they are next touched).

-- 1 task → N activity rows. tenant_id is the CLAUDE.md inegociável (every user table carries it
-- and is isolated by 2 barriers: the app filter + RLS). task_id CASCADEs: the log dies with the
-- task. actor_user_id is the internal app_user id that caused the event (the verified principal,
-- never the body); NOT NULL — an event always has an actor. event_type is the closed set the app
-- validates (TASK_CREATED|TITLE_CHANGED|…). from_value/to_value are the human-readable before/
-- after for a field change (NULL for create/done/dismiss/comment). created_at orders the log.
CREATE TABLE task_activity (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id     uuid NOT NULL REFERENCES tenant(id),
  task_id       uuid NOT NULL REFERENCES task(id) ON DELETE CASCADE,
  actor_user_id uuid NOT NULL,
  event_type    text NOT NULL,
  from_value    text,
  to_value      text,
  created_at    timestamptz NOT NULL DEFAULT now()
);

-- The log reads reverse-chronologically within a task (the detail view scans by task_id ordered
-- by created_at DESC); this index serves that ordered read.
CREATE INDEX task_activity_task_created_idx ON task_activity (task_id, created_at DESC);

-- task_activity is tenant-scoped → same tenant_isolation policy as every user table (barrier 2
-- on top of the app's explicit tenant_id filter, barrier 1). Identical shape to task/task_item.
ALTER TABLE task_activity ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON task_activity
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

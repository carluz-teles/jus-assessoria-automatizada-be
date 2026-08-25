-- 0067_task_comment — the discussion thread of a task (docs/erd-prazos.md §4/§10, the Tarefa
-- detail's "Comentários" tab). A task (the actionable work item, 0024) grows a chronological
-- list of free-text comments written by team members ("Já protocolei", "Aguardando cliente",
-- …). The detail view lists them oldest-first with the author resolved to a name.
--
-- SCHEMA ONLY — zero slice logic (that is internal/deadline). task has rows already; task_comment
-- starts empty (comments are created via the new POST /v1/tasks/:id/comments), so the NOT NULL
-- columns are safe.

-- 1 task → N comments. tenant_id is the CLAUDE.md inegociável (every user table carries it and
-- is isolated by 2 barriers: the app filter + RLS). task_id CASCADEs: a comment has no life of
-- its own — dropping the task drops its thread. author_user_id is the internal app_user id of
-- the writer (the verified principal, never the body); it is NOT NULL — a comment always has an
-- author. body is the free text. created_at orders the thread.
CREATE TABLE task_comment (
  id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id      uuid NOT NULL REFERENCES tenant(id),
  task_id        uuid NOT NULL REFERENCES task(id) ON DELETE CASCADE,
  author_user_id uuid NOT NULL,
  body           text NOT NULL,
  created_at     timestamptz NOT NULL DEFAULT now()
);

-- The thread reads chronologically within a task (the detail view scans by task_id ordered by
-- created_at); this index serves that ordered read.
CREATE INDEX task_comment_task_created_idx ON task_comment (task_id, created_at);

-- task_comment is tenant-scoped → same tenant_isolation policy as every user table (barrier 2
-- on top of the app's explicit tenant_id filter, barrier 1). Identical shape to task/task_item.
ALTER TABLE task_comment ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON task_comment
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

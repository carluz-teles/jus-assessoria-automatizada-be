-- 0031_task_item — the checklist / subtarefas of a task (docs/erd-prazos.md §4/§10,
-- the Tarefas screen). A task (the actionable work item, 0024) grows an ORDERED list of
-- small check items — "Ler intimação", "Analisar documentos", "Redigir", … — each a
-- boolean the user ticks off. The task's progress ({done, total}) and its derived
-- display_status (Em execução once any item is done) read off these rows.
--
-- SCHEMA ONLY — zero slice logic (that is internal/deadline). task has rows already (the
-- F2 confirm + the manual CREATE), but task_item starts empty (items are created via the
-- new POST /v1/tasks/:id/items), so the NOT NULL columns are safe.

-- 1 task → N ordered items. tenant_id is the CLAUDE.md inegociável (every user table
-- carries it and is isolated by 2 barriers: the app filter + RLS). task_id CASCADEs: an
-- item has no life of its own — dropping the task drops its checklist. position orders the
-- checklist (the FE reorders by rewriting positions); done/done_at are the tick state
-- (done_at stamped when done flips true, cleared when it flips back).
CREATE TABLE task_item (
  id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id  uuid NOT NULL REFERENCES tenant(id),
  task_id    uuid NOT NULL REFERENCES task(id) ON DELETE CASCADE,
  title      text NOT NULL,
  position   int  NOT NULL,
  done       boolean NOT NULL DEFAULT false,
  done_at    timestamptz,
  created_at timestamptz NOT NULL DEFAULT now()
);

-- The checklist reads ordered by position within a task (the detail view + the progress
-- count both scan by task_id); this index serves that ordered read.
CREATE INDEX task_item_task_position_idx ON task_item (task_id, position);

-- task_item is tenant-scoped → same tenant_isolation policy as every user table (barrier 2
-- on top of the app's explicit tenant_id filter, barrier 1). Identical shape to task/deadline.
ALTER TABLE task_item ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON task_item
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

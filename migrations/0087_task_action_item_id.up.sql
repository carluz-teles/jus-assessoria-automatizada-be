-- 0087: task.action_item_id — closes the providência → tarefa loop
-- (docs/erd-costura-providencia-tarefa-peca.md §2/§6, fatia 3). §2's cardinality is
-- action_item 1:1 task: when actionitem.created (declarado/manual, born confiável) or
-- actionitem.confirmed (an ia-inferred item just confirmed) fires, the deadline slice's
-- listener creates the task AUTOMATICALLY and stamps this column. internal/actionitem's own
-- task.created listener then reads it back off the event payload (never this column
-- directly — slices never import each other's repo/entity) to write the REVERSE pointer
-- (action_item.task_id, migration 0086) on its own table.
--
-- Nullable: a manual/avulsa task (POST /v1/tasks) never carries one. UNIQUE is the
-- idempotency floor (docs handoff §3): a redelivered actionitem.created/confirmed can never
-- mint a second task for the same providência — InsertTask's ON CONFLICT (action_item_id) DO
-- NOTHING relies on it. Multiple NULLs are always distinct under a plain UNIQUE constraint,
-- so the manual/avulsa path (action_item_id always NULL) is never affected by this guard —
-- action_item_id already gets a unique index from the UNIQUE constraint below, mirroring how
-- migration 0086 notes the same for action_item.task_id.
ALTER TABLE task
  ADD COLUMN action_item_id uuid UNIQUE REFERENCES action_item(id);

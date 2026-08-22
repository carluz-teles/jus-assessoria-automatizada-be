-- 0053_task_priority — add an optional priority to a task (docs/erd-prazos.md §4/§10, the
-- Tarefa detail's "Prioridade" property). A task can be flagged HIGH|MEDIUM|LOW, or carry no
-- priority at all (the column is NULLABLE — "sem prioridade" is a first-class state, the
-- default). The enum is text + app validation (validation.go), mirroring source/kind above
-- (the project convention: text column + CHECK for the closed set the DB also guards).
--
-- SCHEMA ONLY — zero slice logic (that is internal/deadline). task has rows already (the F2
-- confirm + the manual CREATE); the column is NULLABLE with no default, so existing rows keep
-- "sem prioridade" and the ADD is instant (no table rewrite).

ALTER TABLE task
  ADD COLUMN priority text
    CHECK (priority IN ('HIGH', 'MEDIUM', 'LOW'));

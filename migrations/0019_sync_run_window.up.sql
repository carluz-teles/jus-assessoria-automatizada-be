-- 0019_sync_run_window.up.sql — window bounds on sync_run for the reconciliations
-- read model (GET /v1/acquisition/reconciliations): the FE table shows each weekly
-- window ("Janela") per execution, and until now the window lived only in the
-- sync_requested event payload. Nullable: rows predating this migration (and any
-- run not tied to a window) simply have none recorded.
ALTER TABLE sync_run
  ADD COLUMN window_from date,
  ADD COLUMN window_to   date;

-- 0027_intimacao_court_record_idx — the per-process intimations tab (GET
-- /v1/processos/:id/intimacoes) filters intimation by court_record_id and keyset-
-- pages on made_available_at DESC. intimation carries an index on sync_run_id (0020)
-- and its dedup UNIQUE (tenant_id, case_id, hash), but none on court_record_id — so
-- the per-process read would seq-scan the table. This composite serves both the
-- keyset scan and the tab's COUNT. Index-only migration — no data change.
CREATE INDEX IF NOT EXISTS intimation_court_record_id_made_available_at_idx
    ON intimation (court_record_id, made_available_at DESC);

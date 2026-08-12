-- 0026_prazos_read_indexes — the read-side indexes the prazos read models need.
-- Fatia 3 (read models de prazos) adds three GET surfaces over the deadline table:
--   1. GET /v1/processos/:id/prazos — WHERE court_record_id = :id (a process's tab);
--   2. GET /v1/prazos            — WHERE tenant_id ORDER BY end_date (the agenda keyset).
-- 0001 shipped only a partial (end_date) WHERE status='OPEN' index (the expiry sweep) and
-- 0025 added deadline(tenant_id) (RLS/agenda floor). Two gaps remain for these reads:
--   a. deadline.court_record_id — the Prazos tab filters every read by it; without an index
--      the scan grows with the whole tenant's prazos, not just the process's.
--   b. (tenant_id, end_date) — the agenda filters by tenant and orders by end_date; the
--      composite lets the keyset page walk the index in order instead of a sort.
-- Index-only migration — no data change, safe to run anytime. down drops both.

-- The Prazos tab filter column.
CREATE INDEX IF NOT EXISTS deadline_court_record_id_idx ON deadline (court_record_id);

-- The agenda's tenant filter + end_date ordering (keyset pagination) in one index.
CREATE INDEX IF NOT EXISTS deadline_tenant_end_date_idx ON deadline (tenant_id, end_date);

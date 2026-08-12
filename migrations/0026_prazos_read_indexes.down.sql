-- Revert 0026: drop the prazos read-side indexes.
DROP INDEX IF EXISTS deadline_tenant_end_date_idx;
DROP INDEX IF EXISTS deadline_court_record_id_idx;

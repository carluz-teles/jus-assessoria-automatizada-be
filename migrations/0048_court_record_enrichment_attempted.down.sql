-- Reverso da 0048_court_record_enrichment_attempted.
DROP INDEX IF EXISTS court_record_enrichment_scan_idx;
ALTER TABLE court_record DROP COLUMN IF EXISTS enrichment_attempted_at;

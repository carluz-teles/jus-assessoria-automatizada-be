-- claim_value (valor da causa) is structurally dead: no ingestion path (DJEN or
-- DATAJUD) ever writes it — datajudSource has no such field — so the column has been
-- NULL for every row since 0001_init. Drop it end-to-end (BE read models + FE).
ALTER TABLE court_record DROP COLUMN claim_value;

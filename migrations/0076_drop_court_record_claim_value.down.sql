-- Recreate the column nullable, no data to restore (it was always NULL).
ALTER TABLE court_record ADD COLUMN claim_value numeric(15,2);

ALTER TABLE court_record
  DROP COLUMN IF EXISTS magistrate,
  DROP COLUMN IF EXISTS court_situation,
  DROP COLUMN IF EXISTS competence;

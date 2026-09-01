ALTER TABLE court_record
  DROP CONSTRAINT IF EXISTS court_record_phase_check,
  DROP CONSTRAINT IF EXISTS court_record_phase_override_check,
  DROP COLUMN IF EXISTS phase,
  DROP COLUMN IF EXISTS phase_override,
  DROP COLUMN IF EXISTS claim_value;

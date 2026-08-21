DROP INDEX IF EXISTS court_record_judging_body_trgm;
DROP INDEX IF EXISTS court_record_class_trgm;
-- pg_trgm stays: it is a shared extension, other objects may depend on it.

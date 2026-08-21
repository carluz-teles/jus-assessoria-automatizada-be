-- The intimações inbox's ?search now also matches on the court record's class
-- (classe processual) and judging_body (órgão julgador), not just cnj_number — the
-- master-detail redesign lets the user search by either. Same rationale as 0023
-- (pg_trgm's GIN index turns a leading-wildcard ILIKE into an index scan instead of
-- a seq-scan on court_record). pg_trgm is already enabled (0023) — not recreated.
CREATE INDEX court_record_class_trgm
    ON court_record USING GIN (class gin_trgm_ops);

CREATE INDEX court_record_judging_body_trgm
    ON court_record USING GIN (judging_body gin_trgm_ops);

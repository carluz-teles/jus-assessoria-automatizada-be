-- The processes/intimations screens gain a server-side ?search that filters by
-- cnj_number with ILIKE '%term%'. A plain B-tree cannot serve a leading-wildcard
-- match, so it would seq-scan court_record on every keystroke. pg_trgm's GIN index
-- over cnj_number turns that into an index scan (intimations search the JOINed
-- court_record, so this one index serves both the list and the filtered COUNTs).
CREATE EXTENSION IF NOT EXISTS pg_trgm; -- trigram matching for LIKE/ILIKE '%x%'

CREATE INDEX court_record_cnj_number_trgm
    ON court_record USING GIN (cnj_number gin_trgm_ops);

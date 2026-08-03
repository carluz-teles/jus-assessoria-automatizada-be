-- enrichment cycle queries (acquisition slice).
-- DATAJUD enrichment reacts to court_record_observed for a DJEN placeholder
-- (degree=UNKNOWN): it fetches the process by number, reveals the grau, and does
-- the placeholder+merge — find/create the GRADED court_record in the same case,
-- re-point the placeholder's intimations onto it, and mark the placeholder
-- SUPERSEDED. DJEN discovery produces no docket entries, so the placeholder never
-- has any to re-point; DATAJUD's movimentos attach directly to the graded record
-- (InsertDocketEntry, sync.sql).

-- name: UpsertGradedCourtRecord :one
-- Find-or-create the graded court_record on its natural key (tenant, cnj, degree)
-- and refresh the fields DATAJUD is authoritative for. Unlike the discovery path
-- this is NOT gated by the entitlement limit: the process is already tracked (the
-- UNKNOWN placeholder counted against the plan), so grading it must not consume a
-- second slot. case_id is only written on insert (a pre-existing graded record
-- keeps its case); RETURNING carries both so the caller re-points onto this row.
-- next_sync_at is seeded on the INSERT only (the record enters the re-poll
-- schedule when first graded); a refresh (DO UPDATE) leaves it untouched, so the
-- scheduler owns re-scheduling via its claim.
INSERT INTO court_record
    (tenant_id, case_id, cnj_number, degree, court, class, subject, judging_body, filed_at, secrecy, completeness, next_sync_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
ON CONFLICT (tenant_id, cnj_number, degree) DO UPDATE SET
    class = EXCLUDED.class,
    subject = EXCLUDED.subject,
    judging_body = EXCLUDED.judging_body,
    filed_at = EXCLUDED.filed_at,
    secrecy = EXCLUDED.secrecy,
    completeness = EXCLUDED.completeness
RETURNING id, case_id;

-- name: RepointIntimations :execrows
-- Move the placeholder's intimations onto the graded record. Unicidade de
-- intimation é (tenant, case_id, hash), so swapping court_record_id never breaks
-- dedup (same case). Returns the number of rows moved.
UPDATE intimation
SET court_record_id = @to_court_record_id
WHERE tenant_id = @tenant_id AND court_record_id = @from_court_record_id;

-- name: SupersedeCourtRecord :exec
-- Retire the UNKNOWN placeholder after its children moved: it no longer represents
-- a live process (the graded record does), so it drops out of the ACTIVE count and
-- the scheduler (next_sync_at NULL).
UPDATE court_record
SET lifecycle = 'SUPERSEDED', next_sync_at = NULL
WHERE tenant_id = $1 AND id = $2;

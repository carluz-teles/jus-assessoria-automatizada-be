-- enrichment cycle queries (acquisition slice).
-- DATAJUD enrichment reacts to court_record_observed for a DJEN placeholder
-- (degree=UNKNOWN): it fetches the process by number, reveals the grau, and GRADES
-- THE PLACEHOLDER IN PLACE — a single UPDATE that mutates the existing court_record
-- (ev.CourtRecordID) to the real degree + DATAJUD-authoritative fields on the SAME
-- id (UpdateCourtRecordGrade). Because the id never changes, the intimations,
-- deadlines and docket entries already anchored to it stay correct — no orphan, no
-- duplicate (FIX B). The (tenant, cnj, degree) UNIQUE means the ONE case that cannot
-- graduate in place is a rare pre-existing graded record at that grade: the use case
-- detects it first (GetCourtRecordByKey, sync.sql) and MERGES the placeholder into it
-- instead — RepointIntimations moves the placeholder's intimations onto the graded
-- record and SupersedeCourtRecord retires the placeholder (DJEN discovery produces no
-- docket entries, so the placeholder never has any to re-point).

-- name: UpdateCourtRecordGrade :one
-- Grade a court_record IN PLACE: mutate the row named by @court_record_id — the DJEN
-- UNKNOWN placeholder — to the DATAJUD-revealed degree and the fields DATAJUD is
-- authoritative for, keeping the SAME id. Because the id is stable, every child
-- already anchored to it (intimation, deadline, docket_entry) remains correct, which
-- is the whole point of FIX B. It also serves the scheduler re-poll of an
-- already-graded record: the degree is unchanged there (the caller guards the
-- grade-mismatch case), so this just refreshes the authoritative fields. next_sync_at
-- is seeded only when the row still has none (first grade); an existing schedule — the
-- scheduler's own claim — is preserved via COALESCE, so the scheduler keeps owning the
-- re-poll cadence. case_id is never touched (the record keeps its Pasta). RETURNING
-- carries id + case_id for the caller. The (tenant, cnj, degree) UNIQUE requires the
-- caller to ensure no OTHER record already holds @degree (the merge path handles that).
UPDATE court_record SET
    degree = @degree,
    court = @court,
    class = @class,
    subject = @subject,
    judging_body = @judging_body,
    filed_at = @filed_at,
    secrecy = @secrecy,
    completeness = @completeness,
    next_sync_at = COALESCE(next_sync_at, @next_sync_at)
WHERE id = @court_record_id AND tenant_id = @tenant_id
RETURNING id, case_id;

-- name: RepointIntimations :execrows
-- Merge-path only: move the placeholder's intimations onto a PRE-EXISTING graded
-- record when grading in place would violate the (tenant, cnj, degree) UNIQUE.
-- Unicidade de intimation é (tenant, case_id, hash), so swapping court_record_id never
-- breaks dedup (same case). Returns the number of rows moved.
UPDATE intimation
SET court_record_id = @to_court_record_id
WHERE tenant_id = @tenant_id AND court_record_id = @from_court_record_id;

-- name: SupersedeCourtRecord :exec
-- Merge-path only: retire the UNKNOWN placeholder after its intimations moved onto the
-- pre-existing graded record. It no longer represents a live process (the graded record
-- does), so it drops out of the ACTIVE count and the scheduler (next_sync_at NULL).
UPDATE court_record
SET lifecycle = 'SUPERSEDED', next_sync_at = NULL
WHERE tenant_id = $1 AND id = $2;

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
-- lifecycle is derived from DATAJUD movimentos (lifecycleFromMovimentos in the parser):
-- CASE guards that an already-SUPERSEDED row is never downgraded — superseding is a
-- structural merge operation and must survive a scheduler re-poll. When @lifecycle is
-- empty (source does not carry movimentos) the existing lifecycle is preserved via the
-- NULLIF/COALESCE: NULLIF('', lifecycle) = NULL → COALESCE falls back to lifecycle.
UPDATE court_record SET
    degree = @degree,
    court = @court,
    class = @class,
    subject = @subject,
    judging_body = @judging_body,
    filed_at = @filed_at,
    secrecy = @secrecy,
    completeness = @completeness,
    next_sync_at = COALESCE(next_sync_at, @next_sync_at),
    lifecycle = CASE
        WHEN lifecycle = 'SUPERSEDED' THEN lifecycle
        ELSE COALESCE(NULLIF(@lifecycle, ''), lifecycle)
    END
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

-- name: SelectDueForEnrichment :many
-- The batch enrichment's scan: up to @lim court_records of ONE tribunal (@court) that
-- still need enriching — completeness below the DATAJUD ceiling (0.9), never attempted,
-- and not a superseded merge-placeholder. Served by court_record_enrichment_scan_idx
-- (tenant_id, court WHERE the same predicate). ORDER BY id is a STABLE deterministic
-- order (court_record has no created_at column; v0 priority is just "make progress",
-- not recency) so re-runs page the same way; enrichment_attempted_at IS NULL is what
-- makes the scan shrink each step, so a re-delivery re-reads the REMAINING work only.
SELECT id, case_id, cnj_number, degree, court
FROM court_record
WHERE tenant_id = @tenant_id
  AND court = @court
  AND completeness < 0.9
  AND enrichment_attempted_at IS NULL
  AND lifecycle <> 'SUPERSEDED'
ORDER BY id
LIMIT @lim;

-- name: CountRemainingDueForJob :one
-- How many court_records of an ENTIRE import (all tribunals of the backfill_job's tenant)
-- still need enriching. The batch job closes the import's ENRICHMENT capture row ONLY when
-- this reaches 0 — the deterministic fecho: not "updated >= total" (a process DATAJUD never
-- indexed never reaches 0.9), but "no untried record remains". Not-found processes stay
-- marked-attempted-but-<0.9, so they DROP OUT of this count without ever gralduating,
-- which is what stops an infinite loop and lets the fecho land as PARTIAL. Scoped by
-- tenant; the scan index serves the predicate. @backfill_job_id is unused in the filter
-- (the tenant's whole enrichment backlog is the import's, v0 runs one import at a time),
-- kept in the signature so the caller's intent (this import) reads clearly.
SELECT count(*) AS remaining
FROM court_record
WHERE tenant_id = @tenant_id
  AND completeness < 0.9
  AND enrichment_attempted_at IS NULL
  AND lifecycle <> 'SUPERSEDED';

-- name: MarkEnrichmentAttempted :exec
-- Stamp enrichment_attempted_at = @at on EVERY record a batch visited — graded OR not
-- (a number DATAJUD did not return still counts as attempted). This is what removes the
-- record from the scan index so the next step reads fresh work and the fecho can reach
-- 0-remaining. Scoped by tenant (RLS barrier 1). Ids arrive as a uuid[] so one round-trip
-- marks the whole batch.
UPDATE court_record
SET enrichment_attempted_at = @at
WHERE tenant_id = @tenant_id AND id = ANY(@ids::uuid[]);

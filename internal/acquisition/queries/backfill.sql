-- backfill_job queries (acquisition slice).
-- The backfill listener reacts to integration_activated: it opens one backfill_job
-- PER DELTA — the scope's newly-watched OABs (needsHistory from AddOrEnableWatchedOAB,
-- see watched_oab.sql) — and emits their sync slices. There is no longer a whole-
-- integration first-activation guard: adding an OAB to an already-active integration
-- now backfills that OAB's own history instead of being silently swallowed (the bug
-- this replaced). The counters/completion (slices_ok, slices_error, final status)
-- belong to the sync slice, not here.

-- name: InsertBackfillJob :one
-- Create the onboarding backfill job. total_slices is precomputed by the use
-- case; the counters default to zero and status is passed explicitly so a
-- zero-slice horizon can land COMPLETED instead of RUNNING.
INSERT INTO backfill_job
    (tenant_id, integration_id, window_from, window_to, total_slices, status)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id;

-- name: IncrementBackfillSlicesOK :one
-- Count one successful slice and read the job's tallies back atomically. The
-- UPDATE takes a row lock, so concurrent slice closes serialize and the counter
-- never loses a bump; the RETURNING lets the caller decide, in the same tx,
-- whether this was the last slice. Scoped by tenant_id (isolation barrier 1); a
-- non-matching row (wrong tenant / gone) affects zero rows and returns no row.
UPDATE backfill_job
SET slices_ok = slices_ok + 1
WHERE id = $1 AND tenant_id = $2
RETURNING total_slices, slices_ok, slices_error, status;

-- name: IncrementBackfillSlicesError :one
-- Count one failed slice; same atomic lock-and-read-back contract as
-- IncrementBackfillSlicesOK. A job with any failed slice finalizes PARTIAL.
UPDATE backfill_job
SET slices_error = slices_error + 1
WHERE id = $1 AND tenant_id = $2
RETURNING total_slices, slices_ok, slices_error, status;

-- name: FinalizeBackfillJob :exec
-- Flip the job to its terminal status, but ONLY from RUNNING: the WHERE guard
-- makes this the single winning transition, so a late or over-count delivery that
-- reaches here cannot re-finalize. Scoped by tenant_id (isolation barrier 1).
UPDATE backfill_job
SET status = $3
WHERE id = $1 AND tenant_id = $2 AND status = 'RUNNING';

-- name: GetLatestBackfillStatus :one
-- The tenant's most recent backfill job — status + tallies — for the import-status
-- read (the FE banner "importando seus processos…"). Newest job wins (a re-activation
-- opens a new job). No job ever → no row; the read use case maps that to NONE (not
-- importing). Scoped by tenant_id (isolation barrier 1; RLS is barrier 2).
SELECT status, total_slices, slices_ok, slices_error
FROM backfill_job
WHERE tenant_id = $1
ORDER BY created_at DESC
LIMIT 1;

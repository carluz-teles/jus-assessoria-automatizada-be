-- backfill_job queries (acquisition slice).
-- The backfill listener reacts to integration_activated: on the FIRST activation
-- of an integration it creates one backfill_job and emits the sync slices. These
-- two queries are the job's first-activation guard (exists) and its insert; the
-- counters/completion (slices_ok, slices_error, final status) belong to the sync
-- slice, not here.

-- name: BackfillJobExistsByIntegration :one
-- True when a backfill_job already exists for this integration (any status). The
-- listener uses it as the first-activation guard: a re-activation must not
-- re-dispatch a backfill.
SELECT EXISTS (
    SELECT 1 FROM backfill_job WHERE integration_id = $1
) AS job_exists;

-- name: InsertBackfillJob :one
-- Create the onboarding backfill job. total_slices is precomputed by the use
-- case; the counters default to zero and status is passed explicitly so a
-- zero-slice horizon can land COMPLETED instead of RUNNING.
INSERT INTO backfill_job
    (tenant_id, integration_id, window_from, window_to, total_slices, status)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id;

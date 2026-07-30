-- sync cycle queries (acquisition slice).
-- The sync listener reacts to sync_requested: it opens a sync_run (RUNNING),
-- fetches+parses a window, then upserts the observed records/entries/intimations
-- and closes the run (OK/FAILED) — all keyed off the court_record's natural key
-- (tenant_id, cnj_number, degree). Idempotency lives in the schema: docket_entry
-- and intimation carry UNIQUE constraints, so a re-sync of the same window
-- inserts nothing new (ON CONFLICT DO NOTHING) and the RETURNING clause tells the
-- caller which rows were actually new.

-- name: InsertSyncRun :one
-- Open a sync run. court_record_id is left NULL (OAB window discovery is not yet
-- tied to one record); finished_at/error stay NULL until the run closes.
INSERT INTO sync_run
    (tenant_id, integration_id, connector_id, connector_version, started_at, status)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id;

-- name: UpdateSyncRun :exec
-- Close a sync run: OK carries the item tallies and a NULL error; FAILED carries
-- the error jsonb and zero tallies. finished_at is set by the caller's clock.
UPDATE sync_run
SET status = $2,
    items_new = $3,
    items_deduped = $4,
    finished_at = $5,
    error = $6
WHERE id = $1;

-- name: GetCourtRecordByKey :one
-- Resolve a court record by its natural key inside the caller's tx. A miss
-- (pgx.ErrNoRows) is how FindOrCreateCourtRecord learns it must create one.
SELECT id, case_id
FROM court_record
WHERE tenant_id = $1 AND cnj_number = $2 AND degree = $3;

-- name: InsertCourtCase :one
-- Create the lide a first-seen court record hangs on. v0 has no consolidation
-- yet, so every new record gets its own case; merging is a later slice.
INSERT INTO court_case (tenant_id)
VALUES ($1)
RETURNING id;

-- name: InsertCourtRecord :one
-- Create a court record under a case. The natural key (tenant, cnj, degree) is
-- UNIQUE, so a racing double-create fails loudly rather than duplicating.
INSERT INTO court_record
    (tenant_id, case_id, cnj_number, degree, court, class, subject, completeness)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING id;

-- name: MarkCourtRecordSynced :exec
-- Record that a sync touched this court record: refresh its completeness and
-- schedule the next sweep (next_sync_at drives the scheduler slice later).
UPDATE court_record
SET completeness = $2,
    next_sync_at = $3
WHERE id = $1;

-- name: InsertDocketEntry :one
-- Append one andamento, idempotent on (court_record_id, hash). On conflict the
-- row is left untouched and no id is returned (pgx.ErrNoRows), which the caller
-- reads as "deduped" — so a re-sync emits no docket_entry_observed for it.
INSERT INTO docket_entry
    (court_record_id, hash, occurred_at, observed_at, source, fidelity, text)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (court_record_id, hash) DO NOTHING
RETURNING id;

-- name: InsertIntimation :one
-- Append one intimação, idempotent on (tenant_id, case_id, hash). Same
-- conflict-as-dedup contract as docket entries; recipients defaults to '[]'.
INSERT INTO intimation
    (tenant_id, case_id, court_record_id, hash, made_available_at, published_at, deadline_start_at, content, source)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (tenant_id, case_id, hash) DO NOTHING
RETURNING id;

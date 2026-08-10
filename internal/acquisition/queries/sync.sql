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
-- tied to one record); finished_at/error stay NULL until the run closes. event_id
-- records the sync_requested event that opened it, so a re-delivery can find and
-- resume a run that never closed (FindSyncRunByEventID). window_from/to stamp the
-- slice's date window so the reconciliations read can show it (NULL when absent).
INSERT INTO sync_run
    (tenant_id, integration_id, connector_id, connector_version, started_at, status, event_id, window_from, window_to, backfill_job_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING id;

-- name: FindSyncRunByEventID :one
-- Resolve the sync_run opened by a given sync_requested event, inside the caller's
-- (tenant-scoped) tx. On a re-delivery of an already-marked event, the sync use
-- case reads this to decide: a RUNNING run means a prior attempt died before
-- closing it (so the cycle resumes it), while a closed (OK/FAILED) run is a no-op
-- ack. A miss (pgx.ErrNoRows) is the typed ErrSyncRunNotFound.
SELECT id, tenant_id, court_record_id, integration_id, connector_id,
       connector_version, status, items_new, items_deduped, started_at, finished_at
FROM sync_run
WHERE event_id = $1;

-- name: UpdateSyncRun :one
-- Close a sync run, but ONLY from RUNNING: the status guard makes this the single
-- winning transition (compare-and-swap), so a redelivery that resumes a run
-- another execution already closed affects zero rows. OK carries the item tallies
-- and a NULL error; FAILED carries the error jsonb and zero tallies. finished_at
-- is set by the caller's clock. RETURNING id lets the caller learn whether this
-- close won (a row) or lost the race (pgx.ErrNoRows) — the signal to publish the
-- terminal event exactly once. Mirrors FinalizeBackfillJob's RUNNING-only guard.
UPDATE sync_run
SET status = $2,
    items_new = $3,
    items_deduped = $4,
    court_records_new = $5,
    intimations_new = $6,
    finished_at = $7,
    error = $8
WHERE id = $1 AND status = 'RUNNING'
RETURNING id;

-- name: GetCourtRecordByKey :one
-- Resolve a court record by its natural key inside the caller's tx. A miss
-- (pgx.ErrNoRows) is how FindOrCreateCourtRecord learns it must create one.
SELECT id, case_id
FROM court_record
WHERE tenant_id = $1 AND cnj_number = $2 AND degree = $3;

-- name: CountActiveCourtRecordsByTenant :one
-- Count the tenant's ACTIVE court records inside the caller's tx. The entitlement
-- gate reads this against the subscription's active_process_limit before creating a
-- BRAND-NEW record (the MISS path of FindOrCreateCourtRecord only — a reobservation
-- of an existing record is never gated). It runs in the SAME tx as the pending
-- INSERT so the count is consistent with what is about to be created. lifecycle is
-- the schema's process-liveness flag; only ACTIVE records count against the ceiling.
SELECT count(*) FROM court_record
WHERE tenant_id = $1 AND lifecycle = 'ACTIVE';

-- name: InsertCourtCase :one
-- Create the lide a first-seen court record hangs on. v0 has no consolidation
-- yet, so every new record gets its own case; merging is a later slice.
INSERT INTO court_case (tenant_id)
VALUES ($1)
RETURNING id;

-- name: InsertCourtRecord :one
-- Create a court record under a case. The natural key (tenant, cnj, degree) is
-- UNIQUE, so a racing double-create fails loudly rather than duplicating.
-- judging_body (órgão julgador) comes from the source when disclosed (DJEN
-- nomeOrgao / DATAJUD orgaoJulgador), NULL when it does not.
INSERT INTO court_record
    (tenant_id, case_id, cnj_number, degree, court, class, subject, completeness, judging_body, sync_run_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING id;

-- name: MarkCourtRecordSynced :exec
-- Record that a sync touched this court record: refresh its completeness and
-- schedule the next sweep (next_sync_at drives the scheduler slice later).
-- judging_body is COALESCEd, not overwritten: a sync that does not disclose the
-- órgão julgador (NULL) keeps the value a prior sync learned — DATAJUD reveals it
-- after a DJEN discovery landed the record without it (placeholder+merge).
UPDATE court_record
SET completeness = $2,
    next_sync_at = $3,
    judging_body = COALESCE($4, judging_body)
WHERE id = $1;

-- name: InsertDocketEntry :one
-- Append one andamento, idempotent on (court_record_id, hash). On conflict the
-- row is left untouched and no id is returned (pgx.ErrNoRows), which the caller
-- reads as "deduped" — so a re-sync emits no docket_entry_observed for it.
-- tpu_code (Tabela Processual Unificada) and complements are DATAJUD movimento
-- classification; NULL for sources that do not classify the entry.
INSERT INTO docket_entry
    (court_record_id, hash, occurred_at, observed_at, source, fidelity, tpu_code, complements, text)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (court_record_id, hash) DO NOTHING
RETURNING id;

-- name: InsertIntimation :one
-- Append or reconcile one intimação, keyed on (tenant_id, case_id, hash). Unlike
-- docket entries, this is ON CONFLICT DO UPDATE (not DO NOTHING): when the DJEN
-- retracts a publication (data_cancelamento) the SAME hash re-arrives carrying
-- status=CANCELLED + cancelled_at/cancel_reason, and the existing row MUST be
-- updated so the deadline slice revokes the derived prazo — a DO NOTHING would
-- leave a phantom deadline. type/source_url are refreshed alongside. recipients is
-- written on insert and left untouched on update (the matched-OAB flag is stable
-- per hash). (xmax = 0) tells the caller whether THIS upsert inserted a fresh row
-- (true) or updated an existing one (false), so it can still tally new vs. deduped.
INSERT INTO intimation
    (tenant_id, case_id, court_record_id, hash, made_available_at, published_at,
     deadline_start_at, content, source, type, status, source_url, cancelled_at,
     cancel_reason, recipients, sync_run_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
ON CONFLICT (tenant_id, case_id, hash) DO UPDATE SET
    status        = EXCLUDED.status,
    cancelled_at  = EXCLUDED.cancelled_at,
    cancel_reason = EXCLUDED.cancel_reason,
    type          = EXCLUDED.type,
    source_url    = EXCLUDED.source_url
RETURNING id, (xmax = 0) AS inserted;

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
    court_records_updated = $6,
    intimations_new = $7,
    finished_at = $8,
    error = $9
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
-- UNIQUE; ON CONFLICT DO NOTHING makes a concurrent double-create idempotent (two
-- backfill windows discovering the same process at once) — the loser gets no row
-- (pgx.ErrNoRows) and the caller re-resolves the winner instead of a 23505 rollback.
-- judging_body (órgão julgador) comes from the source when disclosed (DJEN
-- nomeOrgao / DATAJUD orgaoJulgador), NULL when it does not.
INSERT INTO court_record
    (tenant_id, case_id, cnj_number, degree, court, class, subject, completeness, judging_body, sync_run_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (tenant_id, cnj_number, degree) DO NOTHING
RETURNING id;

-- name: DeleteCourtCase :exec
-- Drop a court_case orphaned when a concurrent sync won the court_record create
-- race (our InsertCourtRecord hit ON CONFLICT DO NOTHING) — keeps cases 1:1 with
-- records (v0 has no consolidation).
DELETE FROM court_case WHERE id = $1;

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

-- name: AcquireTenantWriteLock :exec
-- Serialize the acquisition domain's WRITES per tenant. The sync and enrichment
-- write transactions take this transaction-scoped advisory lock as their FIRST
-- statement, so two concurrent slices never hold overlapping row/index locks on
-- court_record and thus never deadlock (40P01) — while their slow DJEN/DATAJUD
-- fetches (which run OUTSIDE the tx) still overlap freely. Postgres releases the
-- lock automatically at commit/rollback. The key is hashed from the tenant, so
-- different tenants never block each other; hashtext is int4, widened to the int8
-- pg_advisory_xact_lock expects.
SELECT pg_advisory_xact_lock(hashtext(@tenant_id::text)::bigint);

-- name: GetCourtRecordsByKeys :many
-- Batch counterpart of GetCourtRecordByKey: resolve MANY records by their natural
-- keys in one round-trip, so a sync window partitions its whole result into existing
-- (reobservation → update) vs new (create) with ONE query instead of one GET per
-- record. Keys arrive as a jsonb array of {cnj_number, degree} — one scalar param,
-- so sqlc stays happy (it does not model multi-arg unnest).
SELECT id, case_id, cnj_number, degree
FROM court_record
WHERE tenant_id = @tenant_id
  AND (cnj_number, degree) IN (
    SELECT k.cnj_number, k.degree
    FROM jsonb_to_recordset(@keys::jsonb) AS k(cnj_number text, degree text)
  );

-- name: BatchInsertCourtCases :many
-- Create N fresh lides in one round-trip (v0: one case per new record). Returns the
-- N new ids; the caller pairs each with a new court_record.
INSERT INTO court_case (tenant_id)
SELECT @tenant_id::uuid FROM generate_series(1, @n::int)
RETURNING id;

-- name: BatchInsertCourtRecords :many
-- Bulk create of first-seen records from a jsonb array of rows: folds the post-sync
-- fields (completeness/judging_body/next_sync_at) straight into the INSERT, so a NEW
-- record needs no separate MarkCourtRecordSynced. JSON null → SQL NULL naturally
-- (nullable class/subject/judging_body/sync_run_id). ON CONFLICT DO NOTHING keeps it
-- idempotent (a re-run of the window no-ops on rows it already created); RETURNING the
-- natural key lets the caller map ids back to input order. Under the per-tenant
-- advisory lock there is no concurrent same-tenant writer, so the conflict path only
-- fires on a genuine re-run, never a create race.
INSERT INTO court_record
    (tenant_id, case_id, cnj_number, degree, court, class, subject, completeness, judging_body, sync_run_id, next_sync_at)
SELECT @tenant_id::uuid, r.case_id, r.cnj_number, r.degree, r.court,
       r.class, r.subject, r.completeness, r.judging_body, r.sync_run_id, r.next_sync_at
FROM jsonb_to_recordset(@records::jsonb) AS r(
    case_id uuid, cnj_number text, degree text, court text, class text, subject text,
    completeness real, judging_body text, sync_run_id uuid, next_sync_at timestamptz
)
ON CONFLICT (tenant_id, cnj_number, degree) DO NOTHING
RETURNING id, cnj_number, degree;

-- name: BatchMarkCourtRecordsSynced :exec
-- Bulk MarkCourtRecordSynced for reobserved (already-existing) records: refresh
-- completeness/next_sync_at and COALESCE judging_body/class/subject (a sync that
-- omits them keeps the prior value; a sync that supplies them overwrites — COALESCE on
-- NULLIF('', prior) so an empty string from the source does not clobber a real value).
-- class and subject are included so a tribunal correction in DJEN/DATAJUD propagates
-- on the next re-sync instead of being frozen at the first-seen value.
-- completeness uses GREATEST (never DOWNGRADE): a DJEN re-observation (0.3) must NOT
-- clobber a prior DATAJUD enrichment (0.9). Without this the churn of a backfill
-- re-discovering the same process across windows/OABs erases every enrichment.
UPDATE court_record cr
SET completeness  = GREATEST(u.completeness, cr.completeness),
    next_sync_at  = u.next_sync_at,
    judging_body  = COALESCE(u.judging_body, cr.judging_body),
    class         = COALESCE(NULLIF(u.class, ''), cr.class),
    subject       = COALESCE(NULLIF(u.subject, ''), cr.subject)
FROM jsonb_to_recordset(@updates::jsonb) AS u(
    id uuid, completeness real, next_sync_at timestamptz, judging_body text, class text, subject text
)
WHERE cr.id = u.id;

-- name: BatchUpsertIntimations :many
-- Bulk counterpart of InsertIntimation: upsert MANY intimações in one round-trip from
-- a jsonb array of rows. Same ON CONFLICT (tenant, case, hash) DO UPDATE as the single
-- version (a retracted publication re-arrives with the same hash carrying CANCELLED).
-- JSON handles it all: null → NULL (nullable dates/type/source_url), and recipients
-- (text[]) maps from a JSON array.
--
-- The RETURNING is enriched so the producer can emit the right domain event per row,
-- in the SAME tx, without the deadline slice reading anything back:
--   • inserted  ((xmax = 0)) — a FIRST insert → intimation.observed;
--   • old_status — the status BEFORE this upsert, read by a correlated subquery on
--     `intimation`. A data-modifying CTE and the main query share one snapshot, so
--     the subquery cannot see `upserted`'s effect and returns the PRE-upsert row (a
--     fresh insert has no prior row → NULL → ''). old_status <> 'CANCELLED' + new
--     status = 'CANCELLED' on an UPDATE is the ACTIVE → CANCELLED transition →
--     intimation.cancelled.
--   • court — denormalized from the row's court_record (a join), so the producer
--     derives uf via ufFromTribunal at emission.
WITH upserted AS (
    INSERT INTO intimation
        (tenant_id, case_id, court_record_id, hash, made_available_at, published_at,
         deadline_start_at, content, source, type, status, source_url, cancelled_at,
         cancel_reason, recipients, sync_run_id)
    SELECT @tenant_id::uuid, r.case_id, r.court_record_id, r.hash, r.made_available_at,
           r.published_at, r.deadline_start_at, r.content, r.source, r.type, r.status,
           r.source_url, r.cancelled_at, r.cancel_reason, r.recipients, r.sync_run_id
    FROM jsonb_to_recordset(@intimations::jsonb) AS r(
        case_id uuid, court_record_id uuid, hash text, made_available_at date, published_at date,
        deadline_start_at date, content text, source text, type text, status text, source_url text,
        cancelled_at date, cancel_reason text, recipients jsonb, sync_run_id uuid
    )
    ON CONFLICT (tenant_id, case_id, hash) DO UPDATE SET
        status        = EXCLUDED.status,
        cancelled_at  = EXCLUDED.cancelled_at,
        cancel_reason = EXCLUDED.cancel_reason,
        type          = EXCLUDED.type,
        source_url    = EXCLUDED.source_url
    RETURNING id, court_record_id, type, status, deadline_start_at,
              cancel_reason, (xmax = 0) AS inserted
)
SELECT u.id, u.court_record_id, u.type, u.status, u.deadline_start_at,
       u.cancel_reason, u.inserted, cr.court, cr.case_id,
       COALESCE((SELECT old.status FROM intimation old WHERE old.id = u.id), '')::text AS old_status
FROM upserted u
JOIN court_record cr ON cr.id = u.court_record_id;

-- name: BatchInsertDocketEntries :many
-- Bulk counterpart of InsertDocketEntry: insert MANY andamentos in ONE round-trip from
-- a jsonb array of rows, ON CONFLICT (court_record_id, hash) DO NOTHING (idempotent).
-- Only the rows that were ACTUALLY new are returned (DO NOTHING omits conflicts), so the
-- caller maps them back to build the observed events. This is what keeps the enrichment's
-- per-tenant write lock hold short even for a process with a long movimento history.
INSERT INTO docket_entry
    (court_record_id, hash, occurred_at, observed_at, source, fidelity, tpu_code, complements, text)
SELECT r.court_record_id, r.hash, r.occurred_at, r.observed_at, r.source, r.fidelity, r.tpu_code, r.complements, r.text
FROM jsonb_to_recordset(@entries::jsonb) AS r(
    court_record_id uuid, hash text, occurred_at timestamptz, observed_at timestamptz,
    source text, fidelity int, tpu_code int, complements jsonb, text text
)
ON CONFLICT (court_record_id, hash) DO NOTHING
RETURNING id, court_record_id, hash;

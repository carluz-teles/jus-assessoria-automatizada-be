-- name: AcquireTenantWriteLock :exec
-- Same pattern as internal/acquisition's own AcquireTenantWriteLock (a slice-owned
-- repository operation, not importable across slices) — serializes this slice's
-- writes per tenant so a FetchAutosBatch write tx never deadlocks against another
-- concurrent one. Postgres releases it automatically at commit/rollback.
SELECT pg_advisory_xact_lock(hashtext(@tenant_id::text)::bigint);

-- name: InsertCourtConnection :one
INSERT INTO court_connection (
  tenant_id, app_user_id, court, system, authentication_method,
  credential_ref, certificate_ref, status
) VALUES (
  $1, $2, $3, $4, $5, $6, $7, $8
)
RETURNING id, created_at;

-- name: GetCourtConnectionByID :one
SELECT * FROM court_connection
WHERE id = $1 AND tenant_id = $2;

-- name: UpdateCourtConnectionStatus :one
UPDATE court_connection
SET status = $3,
    last_authenticated_at = $4,
    error = $5
WHERE id = $1 AND tenant_id = $2
RETURNING id;

-- name: UpdateCourtConnectionMFASeedRef :one
UPDATE court_connection
SET mfa_seed_ref = $3
WHERE id = $1 AND tenant_id = $2
RETURNING id;

-- name: ListCourtConnectionsByTenant :many
SELECT * FROM court_connection
WHERE tenant_id = $1
ORDER BY created_at;

-- name: UpdateCourtConnectionSessionRef :one
-- Persists the (possibly renewed) session cookies after a FetchAutos batch.
-- last_authenticated_at is stamped alongside it — a fresh session_ref always means a
-- real (re-)login happened, either at Connect or transparently mid-batch.
UPDATE court_connection
SET session_ref = $3,
    last_authenticated_at = $4
WHERE id = $1 AND tenant_id = $2
RETURNING id;

-- name: FindCourtConnectionByCourtSystem :one
-- v0 simplification: routes by tenant+court+system only, not by which specific
-- advogado a given court_record belongs to (that needs OAB/party-counsel matching
-- that already lives in acquisition and isn't duplicated here yet) — fine while a
-- tenant has at most one connected advogado per tribunal+system; picks the first
-- when there's more than one.
SELECT * FROM court_connection
WHERE tenant_id = $1 AND court = $2 AND system = $3
ORDER BY created_at
LIMIT 1;

-- name: InsertTenantSecret :one
INSERT INTO tenant_secret (tenant_id, ciphertext, nonce, wrapped_dek, dek_nonce)
VALUES ($1, $2, $3, $4, $5)
RETURNING id;

-- name: GetTenantSecretByID :one
SELECT * FROM tenant_secret
WHERE id = $1 AND tenant_id = $2;

-- name: FindOrCreateEprocIntegration :one
-- sync_run.integration_id is NOT NULL (integration is the tenant-level subscription
-- to a source — DJEN/DATAJUD already have one each; EPROC needs its own row purely
-- to satisfy that FK, not the activation/scope machinery acquisition builds on it).
INSERT INTO integration (tenant_id, source, scope)
VALUES ($1, 'EPROC', '{}'::jsonb)
ON CONFLICT (tenant_id, source) DO UPDATE SET source = EXCLUDED.source
RETURNING id;

-- name: InsertCourtSyncRun :one
INSERT INTO sync_run
    (tenant_id, integration_id, connector_id, connector_version, started_at, status)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id;

-- name: UpdateCourtSyncRun :one
-- RUNNING-only guard mirrors acquisition's UpdateSyncRun: the single winning
-- transition, so a redelivered task closing an already-closed run is a no-op.
UPDATE sync_run
SET status = $2,
    items_new = $3,
    items_deduped = $4,
    finished_at = $5,
    error = $6
WHERE id = $1 AND status = 'RUNNING'
RETURNING id;

-- name: UpsertCourtFetchStateObserved :exec
-- The listener's write: a new court_record_observed/intimation.observed event for a
-- record with a matching connection bumps observed_at forward (never backward, so
-- an out-of-order redelivery can't erase a newer observation already recorded).
-- cnj_number only overwrites on insert/conflict since it never changes for a given
-- court_record_id.
INSERT INTO court_fetch_state (tenant_id, court_connection_id, court_record_id, cnj_number, observed_at)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (court_connection_id, court_record_id)
DO UPDATE SET observed_at = GREATEST(court_fetch_state.observed_at, EXCLUDED.observed_at);

-- name: ListDueCourtFetchState :many
-- The use case's pull query (FetchAutosUseCase step 3): due = never fetched, or
-- fetched before the latest observation. Oldest-observed-first so a connection that
-- fell behind catches up in the order events actually arrived.
SELECT court_record_id, cnj_number, observed_at, last_fetched_at, docket_cursor
FROM court_fetch_state
WHERE court_connection_id = $1
  AND (last_fetched_at IS NULL OR last_fetched_at < observed_at)
ORDER BY observed_at ASC
LIMIT $2;

-- name: GetCourtFetchState :one
-- FetchAutosItem's lookup: retrying a SPECIFIC known item (not the batch's due
-- window) still needs its current docket_cursor to pass the same incremental
-- cut FetchAutosBatch would have used.
SELECT court_record_id, cnj_number, observed_at, last_fetched_at, docket_cursor
FROM court_fetch_state
WHERE court_connection_id = $1 AND court_record_id = $2;

-- name: MarkCourtFetchStateFetched :exec
UPDATE court_fetch_state
SET last_fetched_at = $3,
    docket_cursor = $4
WHERE court_connection_id = $1 AND court_record_id = $2;

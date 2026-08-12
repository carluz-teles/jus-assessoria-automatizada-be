-- read-model queries (acquisition slice) — the screen reads, kept OFF the write
-- path (docs: "leitura de tela usa read model, DTO por query dedicada"). Each is
-- tenant-scoped (barrier 1) and keyset-paginated on a stable (sort_key, id) pair
-- so paging is offset-free and stable under concurrent inserts. The caller passes
-- a sentinel cursor for the first page, so there is no conditional WHERE.

-- name: ListProcessos :many
-- The consolidated processes screen: the tenant's live court records (SUPERSEDED
-- placeholders drop out), each with its most recent andamento for the "last
-- movement" column. Ordered by cnj_number then id (ascending keyset): the first
-- page passes ('', zero-uuid).
SELECT cr.id, cr.case_id, cr.cnj_number, cr.court, cr.degree, cr.class, cr.subject,
       cr.judging_body, cr.filed_at, cr.secrecy, cr.lifecycle, cr.completeness,
       COALESCE(m.text, '') AS last_movement_text, m.occurred_at AS last_movement_at
FROM court_record cr
LEFT JOIN LATERAL (
    SELECT de.text, de.occurred_at
    FROM docket_entry de
    WHERE de.court_record_id = cr.id
    ORDER BY de.occurred_at DESC
    LIMIT 1
) m ON true
WHERE cr.tenant_id = $1
  AND cr.lifecycle = 'ACTIVE'
  AND (@search::text = '' OR cr.cnj_number ILIKE '%' || @search || '%' ESCAPE '\')
  AND (cr.cnj_number, cr.id) > (@last_cnj::text, @last_id::uuid)
ORDER BY cr.cnj_number, cr.id
LIMIT $2;

-- name: ListIntimacoes :many
-- The intimações inbox: the tenant's intimations, newest availability first, with
-- the court record's number/court/degree joined in. Descending keyset on
-- (made_available_at, id); the first page passes the max sentinel
-- ('9999-12-31', max-uuid).
SELECT i.id, i.made_available_at, i.published_at, i.deadline_start_at,
       i.content, i.type, i.status, i.source, i.source_url,
       cr.cnj_number, cr.court, cr.degree
FROM intimation i
JOIN court_record cr ON cr.id = i.court_record_id
WHERE i.tenant_id = $1
  AND (@search::text = '' OR cr.cnj_number ILIKE '%' || @search || '%' ESCAPE '\')
  AND (i.made_available_at, i.id) < (@last_made_available::date, @last_id::uuid)
ORDER BY i.made_available_at DESC, i.id DESC
LIMIT $2;

-- name: CountProcessosMatchingSearch :one
-- The filtered "X" of the processes screen's "X de Y" counter: how many ACTIVE court
-- records match the search term (cnj_number ILIKE, trigram-indexed). Called only when
-- ?search is present; the unfiltered "Y" reuses CountActiveCourtRecordsByTenant.
SELECT count(*) FROM court_record cr
WHERE cr.tenant_id = $1
  AND cr.lifecycle = 'ACTIVE'
  AND cr.cnj_number ILIKE '%' || @search::text || '%' ESCAPE '\';

-- name: CountIntimacoesMatchingSearch :one
-- The filtered "X" of the intimations inbox's "X de Y" counter: how many intimations
-- whose court record's cnj_number matches the search term. Called only when ?search
-- is present; the unfiltered "Y" reuses CountIntimationsByTenant.
SELECT count(*) FROM intimation i
JOIN court_record cr ON cr.id = i.court_record_id
WHERE i.tenant_id = $1
  AND cr.cnj_number ILIKE '%' || @search::text || '%' ESCAPE '\';

-- name: ListReconciliations :many
-- The reconciliations screen: one "reconciliação" per import (backfill_job), with
-- the processes/intimations its windows discovered summed up, the job's overall
-- date window (the janela de prazo geral) and slice tallies. finished_at is the
-- last window close once the job is no longer RUNNING (NULL while running).
SELECT b.id, i.source, b.status, b.window_from, b.window_to,
       b.total_slices, b.slices_ok, b.slices_error, b.created_at AS started_at,
       COALESCE(SUM(s.court_records_new), 0)::bigint AS processos,
       COALESCE(SUM(s.intimations_new), 0)::bigint AS intimacoes,
       (CASE WHEN b.status = 'RUNNING' THEN NULL ELSE MAX(s.finished_at) END)::timestamptz AS finished_at
FROM backfill_job b
JOIN integration i ON i.id = b.integration_id
LEFT JOIN sync_run s ON s.backfill_job_id = b.id
WHERE b.tenant_id = $1
GROUP BY b.id, i.source
ORDER BY b.created_at DESC, b.id DESC
LIMIT $2;

-- name: GetReconciliation :one
-- One import's reconciliação header (the detail screen), same shape/aggregation as
-- ListReconciliations but for a single backfill_job.
SELECT b.id, i.source, b.status, b.window_from, b.window_to,
       b.total_slices, b.slices_ok, b.slices_error, b.created_at AS started_at,
       COALESCE(SUM(s.court_records_new), 0)::bigint AS processos,
       COALESCE(SUM(s.intimations_new), 0)::bigint AS intimacoes,
       (CASE WHEN b.status = 'RUNNING' THEN NULL ELSE MAX(s.finished_at) END)::timestamptz AS finished_at
FROM backfill_job b
JOIN integration i ON i.id = b.integration_id
LEFT JOIN sync_run s ON s.backfill_job_id = b.id
WHERE b.tenant_id = $1 AND b.id = $2
GROUP BY b.id, i.source;

-- name: ListSyncRunsByJob :many
-- The windows (sync_runs) of one import, chronological, with the failure reason
-- lifted out of the error jsonb. Drives the detail screen's per-window table and
-- the collapse (each row's id feeds ListProcessos/IntimacoesBySyncRun).
SELECT s.id, i.source, s.window_from, s.window_to, s.status,
       s.court_records_new, s.intimations_new,
       COALESCE(s.error->>'message', '')::text AS error_message,
       s.started_at, s.finished_at
FROM sync_run s
JOIN integration i ON i.id = s.integration_id
WHERE s.tenant_id = $1 AND s.backfill_job_id = $2
ORDER BY s.window_from ASC, s.started_at ASC;

-- name: ListProcessosBySyncRun :many
-- The court records a window first discovered (collapse). Scoped by tenant (RLS +
-- filter) and the discovering sync_run_id; bounded defensively.
SELECT cr.id, cr.cnj_number, cr.court, cr.degree, cr.class
FROM court_record cr
WHERE cr.tenant_id = $1 AND cr.sync_run_id = $2
ORDER BY cr.cnj_number
LIMIT 1000;

-- name: ListIntimacoesBySyncRun :many
-- The intimations a window first discovered (collapse), newest availability first.
SELECT i.id, i.made_available_at, i.type, i.status,
       cr.cnj_number, cr.court, cr.degree
FROM intimation i
JOIN court_record cr ON cr.id = i.court_record_id
WHERE i.tenant_id = $1 AND i.sync_run_id = $2
ORDER BY i.made_available_at DESC, i.id DESC
LIMIT 1000;

-- name: CountIntimationsByTenant :one
-- The reconciliations totals: how many intimations the tenant holds (paired with
-- CountActiveCourtRecordsByTenant for the processes side).
SELECT count(*) FROM intimation WHERE tenant_id = $1;

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
       cr.claim_value,
       -- responsável do processo: assigned at case level (court_case), shared across
       -- graus, so the join is cr → cc → au. Both are LEFT JOINs — an unassigned case
       -- leaves assigned_user_id/name NULL, never dropping the record from the list.
       cc.assigned_user_id, au.name AS assigned_user_name,
       COALESCE(m.text, '') AS last_movement_text, m.occurred_at AS last_movement_at
FROM court_record cr
LEFT JOIN court_case cc ON cc.id = cr.case_id
LEFT JOIN app_user au ON au.id = cc.assigned_user_id
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

-- name: GetProcesso :one
-- One process by id, for the FE deep-link into the processes detail (a process not on
-- the loaded list page). SAME projection as ListProcessos — the record's fields, its
-- last andamento via the LATERAL, and claim_value — so it maps to the same ProcessoView.
-- Scoped by id + tenant_id (barrier 1); NOT filtered by lifecycle, so a SUPERSEDED
-- placeholder is still reachable by direct link. A foreign or unknown id yields no row
-- (→ typed 404 upstream, never nil,nil). Read-only, off the write path.
SELECT cr.id, cr.case_id, cr.cnj_number, cr.court, cr.degree, cr.class, cr.subject,
       cr.judging_body, cr.filed_at, cr.secrecy, cr.lifecycle, cr.completeness,
       cr.claim_value,
       -- responsável do processo, joined at case level (see ListProcessos). LEFT JOINs so
       -- an unassigned case still returns the record with NULL assigned_user_id/name.
       cc.assigned_user_id, au.name AS assigned_user_name,
       COALESCE(m.text, '') AS last_movement_text, m.occurred_at AS last_movement_at
FROM court_record cr
LEFT JOIN court_case cc ON cc.id = cr.case_id
LEFT JOIN app_user au ON au.id = cc.assigned_user_id
LEFT JOIN LATERAL (
    SELECT de.text, de.occurred_at
    FROM docket_entry de
    WHERE de.court_record_id = cr.id
    ORDER BY de.occurred_at DESC
    LIMIT 1
) m ON true
WHERE cr.id = $1 AND cr.tenant_id = $2;

-- name: ListIntimacoes :many
-- The intimações inbox: the tenant's intimations, newest availability first, with
-- the court record's number/court/degree joined in. Descending keyset on
-- (made_available_at, id); the first page passes the max sentinel
-- ('9999-12-31', max-uuid).
SELECT i.id, i.made_available_at, i.published_at, i.deadline_start_at,
       i.content, i.type, i.status, i.user_status, i.source, i.source_url,
       cr.cnj_number, cr.court, cr.degree
FROM intimation i
JOIN court_record cr ON cr.id = i.court_record_id
WHERE i.tenant_id = $1
  AND (@search::text = '' OR cr.cnj_number ILIKE '%' || @search || '%' ESCAPE '\')
  AND (i.made_available_at, i.id) < (@last_made_available::date, @last_id::uuid)
ORDER BY i.made_available_at DESC, i.id DESC
LIMIT $2;

-- name: GetIntimacao :one
-- One intimation by id, for the FE deep-link into the inbox DETAIL screen (an intimation
-- not on the loaded list page). It is a SUPERSET of the list projection: it carries the
-- list fields (so it still maps a full IntimacaoView) PLUS the detail-only extras the
-- inbox row omits —
--   • content       — the FULL teor (not the 500-rune preview the list truncates);
--   • judging_body  — the court record's órgão julgador (court_record.judging_body, 0014);
--   • recipients    — the addressee jsonb list (every destinatário + matched-OAB flag);
--   • user_status   — the triagem state (PENDING|RESOLVED|IGNORED), so the detail can
--                     render the resolve/ignore/reopen affordance.
-- Scoped by tenant_id (barrier 1): a foreign or unknown id yields no row (→ typed 404
-- upstream, never nil,nil). Read-only, off the write path.
SELECT i.id, i.made_available_at, i.published_at, i.deadline_start_at,
       i.content, i.type, i.status, i.user_status, i.source, i.source_url,
       i.recipients, cr.cnj_number, cr.court, cr.degree, cr.judging_body
FROM intimation i
JOIN court_record cr ON cr.id = i.court_record_id
WHERE i.id = $1 AND i.tenant_id = $2;

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

-- name: ListAndamentosByProcesso :many
-- The "Andamentos" tab of one process: the court record's docket entries, newest
-- first. Scoped by tenant via the court_record join (docket_entry has no tenant_id
-- of its own) so a foreign court_record.id passed as :id yields nothing. Descending
-- keyset on (occurred_at, id) — served by docket_entry(court_record_id, occurred_at)
-- — the first page passes the max sentinel ('9999-12-31T23:59:59Z', max-uuid).
SELECT de.id, de.occurred_at, de.observed_at, de.tpu_code, de.text, de.source, de.fidelity
FROM docket_entry de
JOIN court_record cr ON cr.id = de.court_record_id
WHERE de.court_record_id = @court_record_id::uuid
  AND cr.tenant_id = @tenant_id::uuid
  AND (de.occurred_at, de.id) < (@last_occurred::timestamptz, @last_id::uuid)
ORDER BY de.occurred_at DESC, de.id DESC
LIMIT @page_limit;

-- name: CountAndamentosByProcesso :one
-- The "X de Y" total for the Andamentos tab: how many docket entries the process
-- holds. Tenant-scoped through the same court_record join as the list.
SELECT count(*) FROM docket_entry de
JOIN court_record cr ON cr.id = de.court_record_id
WHERE de.court_record_id = @court_record_id::uuid
  AND cr.tenant_id = @tenant_id::uuid;

-- name: ListIntimacoesByProcesso :many
-- The "Intimações" tab of one process: the intimations filed on this court record,
-- newest availability first, with the record's number/court/degree joined in (same
-- projection as ListIntimacoes). Scoped by both court_record_id (the :id) and
-- tenant_id (barrier 1) — intimation has its own tenant_id, so a foreign :id (another
-- tenant's record) matches nothing. Descending keyset on (made_available_at, id) —
-- served by intimation(court_record_id, made_available_at DESC) — the first page
-- passes the max sentinel ('9999-12-31', max-uuid).
SELECT i.id, i.made_available_at, i.published_at, i.deadline_start_at,
       i.content, i.type, i.status, i.user_status, i.source, i.source_url,
       cr.cnj_number, cr.court, cr.degree
FROM intimation i
JOIN court_record cr ON cr.id = i.court_record_id
WHERE i.court_record_id = @court_record_id::uuid
  AND i.tenant_id = @tenant_id::uuid
  AND (i.made_available_at, i.id) < (@last_made_available::date, @last_id::uuid)
ORDER BY i.made_available_at DESC, i.id DESC
LIMIT @page_limit;

-- name: CountIntimacoesByProcesso :one
-- The "X de Y" total for the Intimações tab: how many intimations the process holds.
-- Scoped by the same court_record_id + tenant_id as the list; no court_record join is
-- needed (intimation carries tenant_id), unlike the andamentos count.
SELECT count(*) FROM intimation i
WHERE i.court_record_id = @court_record_id::uuid
  AND i.tenant_id = @tenant_id::uuid;

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

-- name: SummarizeProcessos :one
-- The KPI counts for the processes list header (GET /v1/processos/summary), one
-- aggregate scan over the tenant's court_records grouped into the FE's buckets by
-- court_record.lifecycle (ACTIVE|SUSPENDED|ARCHIVED|SUPERSEDED). SUPERSEDED is the
-- internal UNKNOWN placeholder left after a DATAJUD grade re-point (not a user-facing
-- process), so it is EXCLUDED from every bucket and from `total` — the screen counts
-- only real processes. `baixados` has NO lifecycle source in the v0 schema (there is no
-- BAIXADO value; a baixa would surface as ARCHIVED), so it is always 0 until the schema
-- models it — kept in the projection so the FE contract is stable. tenant-scoped
-- (barrier 1, RLS barrier 2).
SELECT
    count(*) FILTER (WHERE lifecycle <> 'SUPERSEDED')::bigint          AS total,
    count(*) FILTER (WHERE lifecycle = 'ACTIVE')::bigint               AS em_andamento,
    count(*) FILTER (WHERE lifecycle = 'SUSPENDED')::bigint            AS suspensos,
    count(*) FILTER (WHERE lifecycle = 'ARCHIVED')::bigint             AS arquivados,
    0::bigint                                                          AS baixados
FROM court_record
WHERE tenant_id = $1;

-- name: SummarizeIntimacoes :one
-- The KPI counts for the intimações inbox header (GET /v1/intimacoes/summary), one
-- aggregate scan over the tenant's intimations grouped by the triagem state
-- (user_status, 0030) — served by intimation(tenant_id, user_status). pendentes are the
-- rows still awaiting triagem (NOT resolved/ignored, i.e. PENDING); resolvidas=RESOLVED;
-- ignoradas=IGNORED. `em_analise` (an AI-in-progress state) and `criticas` (a
-- deadline-proximity derivation) have no source in this slice yet, so they are 0 until
-- Fase 3 / a prazo derivation lands — kept in the projection so the FE contract is
-- stable. tenant-scoped (barrier 1, RLS barrier 2).
SELECT
    count(*)::bigint                                                   AS total,
    count(*) FILTER (WHERE user_status NOT IN ('RESOLVED', 'IGNORED'))::bigint AS pendentes,
    0::bigint                                                          AS em_analise,
    count(*) FILTER (WHERE user_status = 'RESOLVED')::bigint           AS resolvidas,
    count(*) FILTER (WHERE user_status = 'IGNORED')::bigint            AS ignoradas,
    0::bigint                                                          AS criticas
FROM intimation
WHERE tenant_id = $1;

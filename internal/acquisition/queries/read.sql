-- read-model queries (acquisition slice) — the screen reads, kept OFF the write
-- path (docs: "leitura de tela usa read model, DTO por query dedicada"). Each is
-- tenant-scoped (barrier 1) and keyset-paginated on a stable (sort_key, id) pair
-- so paging is offset-free and stable under concurrent inserts. The caller passes
-- a sentinel cursor for the first page, so there is no conditional WHERE.

-- name: ListProcessos :many
-- The consolidated processes screen: the tenant's live court records (SUPERSEDED
-- placeholders drop out), each with its most recent andamento for the "last
-- movement" column. Ordered by cnj_number then id (ascending keyset): the first
-- page passes ('', zero-uuid). Optional filters — @court / @degree (free text from
-- the DISTINCT options), @lifecycle (a closed set; '' keeps the default ACTIVE
-- context), @assignee_id (NULL = any; the case-level responsável) — are additive
-- ANDs, so an absent filter matches everything.
-- next_deadline: the nearest OPEN/PENDING prazo of the process, derived via a
-- correlated LATERAL (LIMIT 1 — one row per process, keyset stays N rows = N
-- processes). The path is court_record → intimation → deadline (notification_id
-- is the historic FK name for intimation_id). NULL when no OPEN/PENDING prazo exists.
-- CASE WHEN d.id IS NOT NULL guards all nullable deadline expressions (mirrors the
-- ListIntimacoes pattern in this file) so sqlc infers nullable types correctly.
SELECT cr.id, cr.case_id, cr.cnj_number, cr.court, cr.degree, cr.class, cr.subject,
       cr.judging_body, cr.filed_at, cr.secrecy, cr.lifecycle, cr.completeness,
       -- fase EFETIVA (override manual vence a derivada) + valor da causa (manual).
       COALESCE(cr.phase_override, cr.phase) AS phase, cr.claim_value,
       -- responsável do processo: assigned at case level (court_case), shared across
       -- graus, so the join is cr → cc → au. Both are LEFT JOINs — an unassigned case
       -- leaves assigned_user_id/name NULL, never dropping the record from the list.
       cc.assigned_user_id, au.name AS assigned_user_name,
       COALESCE(m.text, '') AS last_movement_text, m.occurred_at AS last_movement_at,
       -- prazo mais próximo ABERTO do processo (nil = JSON null when absent).
       nd.end_date AS next_deadline_end_date,
       CASE WHEN nd.end_date IS NOT NULL THEN (nd.end_date - CURRENT_DATE)::int END AS next_deadline_days_left,
       nd.kind    AS next_deadline_kind
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
LEFT JOIN LATERAL (
    SELECT d.end_date, d.kind
    FROM deadline d
    JOIN intimation i ON i.id = d.notification_id
    WHERE i.court_record_id = cr.id
      AND d.tenant_id = cr.tenant_id
      AND d.status IN ('OPEN', 'PENDING')
    ORDER BY d.end_date ASC
    LIMIT 1
) nd ON true
WHERE cr.tenant_id = $1
  AND (@search::text = '' OR cr.cnj_number ILIKE '%' || @search || '%' ESCAPE '\')
  AND (@court::text = '' OR cr.court = @court::text)
  AND (@degree::text = '' OR cr.degree = @degree::text)
  AND (@lifecycle::text = '' OR cr.lifecycle = @lifecycle::text)
  AND (@lifecycle::text <> '' OR cr.lifecycle = 'ACTIVE')
  AND (sqlc.narg('assignee_id')::uuid IS NULL OR cc.assigned_user_id = sqlc.narg('assignee_id')::uuid)
  AND (cr.cnj_number, cr.id) > (@last_cnj::text, @last_id::uuid)
ORDER BY cr.cnj_number, cr.id
LIMIT $2;

-- name: GetProcesso :one
-- One process by id, for the FE deep-link into the processes detail (a process not on
-- the loaded list page). SAME projection as ListProcessos — the record's fields and its
-- last andamento via the LATERAL — so it maps to the same ProcessoView.
-- Scoped by id + tenant_id (barrier 1); NOT filtered by lifecycle, so a SUPERSEDED
-- placeholder is still reachable by direct link. A foreign or unknown id yields no row
-- (→ typed 404 upstream, never nil,nil). Read-only, off the write path.
-- next_deadline: same correlated LATERAL as ListProcessos (see above).
SELECT cr.id, cr.case_id, cr.cnj_number, cr.court, cr.degree, cr.class, cr.subject,
       cr.judging_body, cr.filed_at, cr.secrecy, cr.lifecycle, cr.completeness,
       -- fase EFETIVA (override manual vence a derivada) + valor da causa (manual).
       COALESCE(cr.phase_override, cr.phase) AS phase, cr.claim_value,
       -- responsável do processo, joined at case level (see ListProcessos). LEFT JOINs so
       -- an unassigned case still returns the record with NULL assigned_user_id/name.
       cc.assigned_user_id, au.name AS assigned_user_name,
       COALESCE(m.text, '') AS last_movement_text, m.occurred_at AS last_movement_at,
       -- prazo mais próximo ABERTO do processo (nil = JSON null when absent).
       nd.end_date AS next_deadline_end_date,
       CASE WHEN nd.end_date IS NOT NULL THEN (nd.end_date - CURRENT_DATE)::int END AS next_deadline_days_left,
       nd.kind    AS next_deadline_kind
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
LEFT JOIN LATERAL (
    SELECT d.end_date, d.kind
    FROM deadline d
    JOIN intimation i ON i.id = d.notification_id
    WHERE i.court_record_id = cr.id
      AND d.tenant_id = cr.tenant_id
      AND d.status IN ('OPEN', 'PENDING')
    ORDER BY d.end_date ASC
    LIMIT 1
) nd ON true
WHERE cr.id = $1 AND cr.tenant_id = $2;

-- name: ListIntimacoes :many
-- The intimações inbox: the tenant's intimations, newest availability first, with
-- the court record's number/court/degree joined in. Descending keyset on
-- (made_available_at, id); the first page passes the max sentinel
-- ('9999-12-31', max-uuid). Optional filters — @court (free text from the DISTINCT
-- options), @type / @user_status (closed sets), @urgencia (deadline-proximity /
-- providência bucket, closed set), @search (cnj_number/class/judging_body ILIKE),
-- @assignee_id (the "Minhas" toggle) — are additive ANDs. The LEFT JOIN deadline
-- exposes the derived prazo (1:1 per notification_id, UNIQUE); NULL when no prazo
-- exists yet. ai_analyzed_at + assignee_user_id/name mirror GetIntimacao's
-- projection (same LEFT JOIN app_user pattern) so the inbox row can render the
-- "não analisada" badge and the responsável label without a deep-link.
SELECT i.id, i.made_available_at, i.published_at, i.deadline_start_at,
       i.content, i.type, i.status, i.user_status, i.source, i.source_url,
       cr.cnj_number, cr.court, cr.degree, cr.class, cr.subject, cr.id AS court_record_id,
       i.ai_analyzed_at, i.assignee_user_id, ua.name AS assignee_user_name,
       -- prazo derivado (nullable — nem toda intimação tem prazo ainda). CASE WHEN
       -- garante que todas as expressões derivadas sejam NULL quando não há prazo
       -- (LEFT JOIN miss), forçando o sqlc a inferir os tipos como nullable (*T).
       d.id                                                                       AS prazo_deadline_id,
       d.end_date                                                                 AS prazo_end_date,
       CASE WHEN d.id IS NOT NULL THEN (d.end_date - CURRENT_DATE)::int END       AS prazo_days_left,
       d.status                                                                   AS prazo_status,
       CASE WHEN d.id IS NOT NULL THEN (d.confirmed_by IS NOT NULL) END           AS prazo_confirmed
FROM intimation i
JOIN court_record cr ON cr.id = i.court_record_id
LEFT JOIN deadline d ON d.notification_id = i.id AND d.tenant_id = i.tenant_id
LEFT JOIN app_user ua ON ua.id = i.assignee_user_id
WHERE i.tenant_id = $1
  AND (
    @search::text = ''
    OR cr.cnj_number ILIKE '%' || @search || '%' ESCAPE '\'
    OR cr.class ILIKE '%' || @search || '%'
    OR cr.judging_body ILIKE '%' || @search || '%'
  )
  AND (@type::text = '' OR i.type = @type::text)
  AND (@user_status::text = '' OR i.user_status = @user_status::text)
  AND (@court::text = '' OR cr.court = @court::text)
  AND (
    sqlc.narg('assignee_id')::uuid IS NULL
    OR i.assignee_user_id = sqlc.narg('assignee_id')::uuid
  )
  AND (
    @urgencia::text = ''
    OR (@urgencia::text = 'atraso'             AND (d.end_date - CURRENT_DATE) < 0  AND d.status IN ('PENDING', 'OPEN') AND i.user_status NOT IN ('RESOLVED', 'IGNORED'))
    OR (@urgencia::text = 'hoje'               AND (d.end_date - CURRENT_DATE) = 0  AND d.status IN ('PENDING', 'OPEN') AND i.user_status NOT IN ('RESOLVED', 'IGNORED'))
    OR (@urgencia::text = 'proximos_dois_dias' AND (d.end_date - CURRENT_DATE) BETWEEN 1 AND 2 AND d.status IN ('PENDING', 'OPEN') AND i.user_status NOT IN ('RESOLVED', 'IGNORED'))
    OR (@urgencia::text = 'semana'             AND (d.end_date - CURRENT_DATE) BETWEEN 3 AND 7 AND d.status IN ('PENDING', 'OPEN') AND i.user_status NOT IN ('RESOLVED', 'IGNORED'))
    OR (@urgencia::text = 'este_mes'           AND (d.end_date - CURRENT_DATE) > 7 AND d.end_date <= (date_trunc('month', CURRENT_DATE) + interval '1 month' - interval '1 day')::date AND d.status IN ('PENDING', 'OPEN') AND i.user_status NOT IN ('RESOLVED', 'IGNORED'))
    OR (@urgencia::text = 'mais_adiante'       AND d.end_date > (date_trunc('month', CURRENT_DATE) + interval '1 month' - interval '1 day')::date AND d.status IN ('PENDING', 'OPEN') AND i.user_status NOT IN ('RESOLVED', 'IGNORED'))
    OR (@urgencia::text = 'sem_data_definida'  AND d.id IS NULL AND i.user_status NOT IN ('RESOLVED', 'IGNORED'))
  )
  -- "Não confirmadas" triage toggle (?nao_confirmado): server-side, combines with any
  -- temporal tab. Reuses the former nao_confirmado predicate — a suggested (not yet
  -- human-confirmed) deadline has d.status = 'PENDING'. When off, no extra filter.
  AND (@nao_confirmado::bool = false OR d.status = 'PENDING')
  AND (i.made_available_at, i.id) < (@last_made_available::date, @last_id::uuid)
ORDER BY i.made_available_at DESC, i.id DESC
LIMIT $2;

-- name: GetIntimacao :one
-- One intimation by id, for the FE deep-link into the inbox DETAIL screen (an intimation
-- not on the loaded list page). It is a SUPERSET of the list projection: it carries the
-- list fields (so it still maps a full IntimacaoView, including the prazo block) PLUS the
-- detail-only extras the inbox row omits —
--   • content                — the FULL teor (not the 500-rune preview the list truncates);
--   • judging_body           — the court record's órgão julgador (court_record.judging_body);
--   • recipients             — the addressee jsonb list (every destinatário + matched-OAB flag);
--   • user_status            — the triagem state (PENDING|RESOLVED|IGNORED);
--   • assignee_user_id/name  — responsável pela intimação (nullable, LEFT JOIN app_user);
--   • deadline.confirmed_at  — when the deadline was confirmed (for history derivation);
--   • deadline.confirmed_by_name — confirmer's name (for history label; nullable);
--   • deadline.status (already present) — for history label (OPEN/NO_DEADLINE branch).
-- The LEFT JOIN deadline mirrors ListIntimacoes (same 1:1 notification_id join); NULL
-- when the intimação has no prazo yet. Scoped by tenant_id (barrier 1): a foreign or
-- unknown id yields no row (→ typed 404 upstream, never nil,nil). Read-only, off the
-- write path.
SELECT i.id, i.made_available_at, i.published_at, i.deadline_start_at,
       i.content, i.type, i.status, i.user_status, i.source, i.source_url,
       i.recipients, cr.cnj_number, cr.court, cr.degree, cr.class, cr.subject, cr.id AS court_record_id, cr.judging_body,
       (SELECT array_agg(p.name ORDER BY p.name)
          FROM party p
         WHERE p.case_id = cr.case_id
           AND p.tenant_id = cr.tenant_id
           AND p.role = 'PLAINTIFF')::text[]                                        AS plaintiffs,
       (SELECT array_agg(p.name ORDER BY p.name)
          FROM party p
         WHERE p.case_id = cr.case_id
           AND p.tenant_id = cr.tenant_id
           AND p.role = 'DEFENDANT')::text[]                                        AS defendants,
       -- data de distribuição/ajuizamento (nullable — DJEN não carrega, só o
       -- enriquecimento DATAJUD preenche via migration 0015). Exposto no
       -- detail pra alimentar a linha "Distribuição" da barra rica.
       cr.filed_at AS distribution_date,
       -- análise IA (0051): NULLs = pré-análise; ai_analyzed_at NOT NULL = pós-análise.
       i.ai_summary, i.ai_providencias, i.ai_analyzed_at,
       -- responsável (0057, ex-conductor/reviewer): nullable — id + name via LEFT JOIN app_user.
       i.assignee_user_id,
       ua.name                                                                     AS assignee_user_name,
       -- prazo derivado (nullable — nem toda intimação tem prazo ainda). CASE WHEN
       -- garante que todas as expressões derivadas sejam NULL quando não há prazo
       -- (LEFT JOIN miss), forçando o sqlc a inferir os tipos como nullable (*T).
       d.id                                                                       AS prazo_deadline_id,
       d.end_date                                                                 AS prazo_end_date,
       CASE WHEN d.id IS NOT NULL THEN (d.end_date - CURRENT_DATE)::int END       AS prazo_days_left,
       d.status                                                                   AS prazo_status,
       CASE WHEN d.id IS NOT NULL THEN (d.confirmed_by IS NOT NULL) END           AS prazo_confirmed,
       -- histórico derivado: confirmed_at + confirmer name (para label "Prazo confirmado por X")
       d.confirmed_at                                                             AS prazo_confirmed_at,
       ucf.name                                                                   AS prazo_confirmed_by_name
FROM intimation i
JOIN court_record cr ON cr.id = i.court_record_id
LEFT JOIN deadline d ON d.notification_id = i.id AND d.tenant_id = i.tenant_id
LEFT JOIN app_user ua  ON ua.id = i.assignee_user_id
LEFT JOIN app_user ucf ON ucf.id = d.confirmed_by
WHERE i.id = $1 AND i.tenant_id = $2;

-- name: CountProcessosFiltered :one
-- The filtered "X" of the processes screen's "X de Y" counter: how many court records
-- match the active filters (search on cnj_number ILIKE, court, degree, lifecycle, and
-- the case-level responsável). Called only when any filter is present; the unfiltered
-- "Y" is CountActiveCourtRecordsByTenant (or CountCourtRecordsByLifecycle when
-- ?lifecycle is set). The SAME predicates as ListProcessos (minus the keyset), so the
-- counter agrees with the page. The LEFT JOIN court_case is needed for the assignee
-- predicate; the case id is the join key, so it never multiplies rows.
SELECT count(*) FROM court_record cr
LEFT JOIN court_case cc ON cc.id = cr.case_id
WHERE cr.tenant_id = $1
  AND (@search::text = '' OR cr.cnj_number ILIKE '%' || @search || '%' ESCAPE '\')
  AND (@court::text = '' OR cr.court = @court::text)
  AND (@degree::text = '' OR cr.degree = @degree::text)
  AND (@lifecycle::text = '' OR cr.lifecycle = @lifecycle::text)
  AND (@lifecycle::text <> '' OR cr.lifecycle = 'ACTIVE')
  AND (sqlc.narg('assignee_id')::uuid IS NULL OR cc.assigned_user_id = sqlc.narg('assignee_id')::uuid);

-- name: CountCourtRecordsByLifecycle :one
-- The "Y" of the counter when the user filters ?lifecycle: how many records the tenant
-- holds in that lifecycle, so "X de Y" stays meaningful (X ⊆ that lifecycle). ACTIVE —
-- the screen's default context — keeps the cheaper CountActiveCourtRecordsByTenant.
SELECT count(*) FROM court_record WHERE tenant_id = $1 AND lifecycle = $2;

-- name: CountIntimacoesFiltered :one
-- The filtered "X" of the intimations inbox's "X de Y" counter: how many intimations
-- match the active filters (search on the court record's cnj_number/class/judging_body,
-- type, user_status, court, urgencia, assignee). Called only when a filter is present;
-- the unfiltered "Y" reuses CountIntimationsByTenant. SAME predicates as ListIntimacoes
-- (minus the keyset). The LEFT JOIN deadline mirrors ListIntimacoes so the urgencia
-- filter agrees with the page.
SELECT count(*) FROM intimation i
JOIN court_record cr ON cr.id = i.court_record_id
LEFT JOIN deadline d ON d.notification_id = i.id AND d.tenant_id = i.tenant_id
WHERE i.tenant_id = $1
  AND (
    @search::text = ''
    OR cr.cnj_number ILIKE '%' || @search || '%' ESCAPE '\'
    OR cr.class ILIKE '%' || @search || '%'
    OR cr.judging_body ILIKE '%' || @search || '%'
  )
  AND (@type::text = '' OR i.type = @type::text)
  AND (@user_status::text = '' OR i.user_status = @user_status::text)
  AND (@court::text = '' OR cr.court = @court::text)
  AND (
    sqlc.narg('assignee_id')::uuid IS NULL
    OR i.assignee_user_id = sqlc.narg('assignee_id')::uuid
  )
  AND (
    @urgencia::text = ''
    OR (@urgencia::text = 'atraso'             AND (d.end_date - CURRENT_DATE) < 0  AND d.status IN ('PENDING', 'OPEN') AND i.user_status NOT IN ('RESOLVED', 'IGNORED'))
    OR (@urgencia::text = 'hoje'               AND (d.end_date - CURRENT_DATE) = 0  AND d.status IN ('PENDING', 'OPEN') AND i.user_status NOT IN ('RESOLVED', 'IGNORED'))
    OR (@urgencia::text = 'proximos_dois_dias' AND (d.end_date - CURRENT_DATE) BETWEEN 1 AND 2 AND d.status IN ('PENDING', 'OPEN') AND i.user_status NOT IN ('RESOLVED', 'IGNORED'))
    OR (@urgencia::text = 'semana'             AND (d.end_date - CURRENT_DATE) BETWEEN 3 AND 7 AND d.status IN ('PENDING', 'OPEN') AND i.user_status NOT IN ('RESOLVED', 'IGNORED'))
    OR (@urgencia::text = 'este_mes'           AND (d.end_date - CURRENT_DATE) > 7 AND d.end_date <= (date_trunc('month', CURRENT_DATE) + interval '1 month' - interval '1 day')::date AND d.status IN ('PENDING', 'OPEN') AND i.user_status NOT IN ('RESOLVED', 'IGNORED'))
    OR (@urgencia::text = 'mais_adiante'       AND d.end_date > (date_trunc('month', CURRENT_DATE) + interval '1 month' - interval '1 day')::date AND d.status IN ('PENDING', 'OPEN') AND i.user_status NOT IN ('RESOLVED', 'IGNORED'))
    OR (@urgencia::text = 'sem_data_definida'  AND d.id IS NULL AND i.user_status NOT IN ('RESOLVED', 'IGNORED'))
  )
  AND (@nao_confirmado::bool = false OR d.status = 'PENDING');

-- ── filter options (the envelope's selectable sets) ──────────────────────────
-- Distinct-value reads that back the list envelopes' filter chips. Each is
-- tenant-scoped (barrier 1) and matches the list's OWN context predicate (the
-- processos options restrict to the default ACTIVE lifecycle), so the options
-- reflect what the list can actually show. Empty values are skipped in Go (a blank
-- chip is never selectable).

-- name: ListProcessoCourts :many
-- Selectable ?court values for the processes screen: the distinct courts of the
-- tenant's live (ACTIVE) records, ordered by name (case-insensitive). The DISTINCT
-- lives in the inner query — a bare SELECT DISTINCT with ORDER BY on an expression not
-- in the select list is rejected by Postgres — and the outer layer orders.
SELECT court
FROM (
  SELECT DISTINCT cr.court
  FROM court_record cr
  WHERE cr.tenant_id = $1
    AND cr.lifecycle = 'ACTIVE'
) courts
ORDER BY LOWER(court) ASC;

-- name: ListProcessoDegrees :many
-- Selectable ?degree values for the processes screen: the distinct degrees of the
-- tenant's live (ACTIVE) records, ordered by name (case-insensitive).
SELECT degree
FROM (
  SELECT DISTINCT cr.degree
  FROM court_record cr
  WHERE cr.tenant_id = $1
    AND cr.lifecycle = 'ACTIVE'
) degrees
ORDER BY LOWER(degree) ASC;

-- name: ListProcessoAssignees :many
-- Selectable ?assignee values for the processes screen: the responsáveis of the
-- tenant's live processes, joined at case level (court_record → court_case →
-- app_user) exactly like the list's projection, deduped by id, ordered by name
-- (case-insensitive). The list filters on cc.assigned_user_id, so an unassigned case
-- yields no option.
SELECT id, name
FROM (
  SELECT DISTINCT au.id, COALESCE(au.name, '') AS name
  FROM court_record cr
  JOIN court_case cc ON cc.id = cr.case_id
  JOIN app_user au ON au.id = cc.assigned_user_id
  WHERE cr.tenant_id = $1
    AND cr.lifecycle = 'ACTIVE'
) assignees
ORDER BY LOWER(name) ASC;

-- name: ListIntimacaoCourts :many
-- Selectable ?court values for the intimações inbox: the distinct courts of the
-- tenant's intimated court records, ordered by name (case-insensitive).
SELECT court
FROM (
  SELECT DISTINCT cr.court
  FROM intimation i
  JOIN court_record cr ON cr.id = i.court_record_id
  WHERE i.tenant_id = $1
) courts
ORDER BY LOWER(court) ASC;

-- name: ListAndamentosByProcesso :many
-- The "Andamentos" tab of one process: the court record's docket entries, newest
-- first. Scoped by tenant via the court_record join (docket_entry has no tenant_id
-- of its own) so a foreign court_record.id passed as :id yields nothing. Descending
-- keyset on (occurred_at, id) — served by docket_entry(court_record_id, occurred_at)
-- — the first page passes the max sentinel ('9999-12-31T23:59:59Z', max-uuid).
SELECT de.id, de.occurred_at, de.observed_at, de.tpu_code, de.text, de.complements, de.source, de.fidelity
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
-- MESMA projeção do ListIntimacoes (inbox): traz o responsável (assignee_user_id/name)
-- e o prazo derivado (LEFT JOINs deadline + app_user), pra o card de intimação do cockpit
-- renderizar "resp." e o prazo/fatal como no design.
SELECT i.id, i.made_available_at, i.published_at, i.deadline_start_at,
       i.content, i.type, i.status, i.user_status, i.source, i.source_url,
       cr.cnj_number, cr.court, cr.degree, cr.class, cr.subject, cr.id AS court_record_id,
       i.ai_analyzed_at, i.assignee_user_id, ua.name AS assignee_user_name,
       d.id                                                                 AS prazo_deadline_id,
       d.end_date                                                           AS prazo_end_date,
       CASE WHEN d.id IS NOT NULL THEN (d.end_date - CURRENT_DATE)::int END AS prazo_days_left,
       d.status                                                             AS prazo_status,
       CASE WHEN d.id IS NOT NULL THEN (d.confirmed_by IS NOT NULL) END     AS prazo_confirmed
FROM intimation i
JOIN court_record cr ON cr.id = i.court_record_id
LEFT JOIN deadline d ON d.notification_id = i.id AND d.tenant_id = i.tenant_id
LEFT JOIN app_user ua ON ua.id = i.assignee_user_id
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
-- aggregate scan over the tenant's intimations. The LEFT JOIN deadline (1:1 per
-- notification_id) enables the urgência buckets without a second scan.
--   pendentes      = triagem PENDING (awaiting user decision)
--   em_atraso      = prazo vencido (days_left < 0) AND status IN (PENDING, OPEN)
--   vencem_hoje    = prazo vence hoje (days_left = 0) AND status IN (PENDING, OPEN)
--   nao_confirmado = prazo sugerido ainda não confirmado (status = 'PENDING')
-- `em_analise` (AI-in-progress) has no source yet — kept at 0 so the FE contract is
-- stable. `criticas` is retired (replaced by em_atraso/vencem_hoje/nao_confirmado),
-- kept at 0 for backward-compat. tenant-scoped (barrier 1, RLS barrier 2).
SELECT
    count(*)::bigint                                                                                                        AS total,
    count(*) FILTER (WHERE i.user_status NOT IN ('RESOLVED', 'IGNORED'))::bigint                                           AS pendentes,
    0::bigint                                                                                                               AS em_analise,
    count(*) FILTER (WHERE i.user_status = 'RESOLVED')::bigint                                                             AS resolvidas,
    count(*) FILTER (WHERE i.user_status = 'IGNORED')::bigint                                                              AS ignoradas,
    0::bigint                                                                                                               AS criticas,
    count(*) FILTER (WHERE (d.end_date - CURRENT_DATE) < 0  AND d.status IN ('PENDING', 'OPEN'))::bigint                   AS em_atraso,
    count(*) FILTER (WHERE (d.end_date - CURRENT_DATE) = 0  AND d.status IN ('PENDING', 'OPEN'))::bigint                   AS vencem_hoje,
    count(*) FILTER (WHERE d.status = 'PENDING')::bigint                                                                   AS nao_confirmado
FROM intimation i
LEFT JOIN deadline d ON d.notification_id = i.id AND d.tenant_id = i.tenant_id
WHERE i.tenant_id = $1;

-- name: CountIntimacoesBuckets :one
-- Bucket counts for the list envelope's `buckets` object, derived in the same
-- request as ListIntimacoes (no N+1). Mirrors the urgência buckets of the list's
-- WHERE clause but expressed as FILTER aggregates over the SAME non-urgência
-- filters (type, user_status, court, search — assignee is deliberately NOT
-- applied here, mirroring how the processes screen's other chips don't gate its
-- own filter options) — so the header badges agree with what the list would show
-- when the user picks that bucket. The seven buckets are mutually disjoint and
-- all exclude user_status RESOLVED/IGNORED. The four+ temporal buckets restrict
-- to status IN (PENDING, OPEN): MISSED/MET deadlines belong to closed intimations
-- and are not counted as actionable. `sem_data_definida` counts intimations with
-- no derived deadline (deadline LEFT JOIN miss). The buckets do NOT apply the
-- `nao_confirmado` triage toggle (it is a per-list filter, independent of tabs).
SELECT
    count(*) FILTER (WHERE (d.end_date - CURRENT_DATE) < 0   AND d.status IN ('PENDING', 'OPEN') AND i.user_status NOT IN ('RESOLVED', 'IGNORED'))::bigint AS atraso,
    count(*) FILTER (WHERE (d.end_date - CURRENT_DATE) = 0   AND d.status IN ('PENDING', 'OPEN') AND i.user_status NOT IN ('RESOLVED', 'IGNORED'))::bigint AS hoje,
    count(*) FILTER (WHERE (d.end_date - CURRENT_DATE) BETWEEN 1 AND 2 AND d.status IN ('PENDING', 'OPEN') AND i.user_status NOT IN ('RESOLVED', 'IGNORED'))::bigint AS proximos_dois_dias,
    count(*) FILTER (WHERE (d.end_date - CURRENT_DATE) BETWEEN 3 AND 7 AND d.status IN ('PENDING', 'OPEN') AND i.user_status NOT IN ('RESOLVED', 'IGNORED'))::bigint AS esta_semana,
    count(*) FILTER (WHERE (d.end_date - CURRENT_DATE) > 7 AND d.end_date <= (date_trunc('month', CURRENT_DATE) + interval '1 month' - interval '1 day')::date AND d.status IN ('PENDING', 'OPEN') AND i.user_status NOT IN ('RESOLVED', 'IGNORED'))::bigint AS este_mes,
    count(*) FILTER (WHERE d.end_date > (date_trunc('month', CURRENT_DATE) + interval '1 month' - interval '1 day')::date AND d.status IN ('PENDING', 'OPEN') AND i.user_status NOT IN ('RESOLVED', 'IGNORED'))::bigint AS mais_adiante,
    count(*) FILTER (WHERE d.id IS NULL AND i.user_status NOT IN ('RESOLVED', 'IGNORED'))::bigint AS sem_data_definida
FROM intimation i
JOIN court_record cr ON cr.id = i.court_record_id
LEFT JOIN deadline d ON d.notification_id = i.id AND d.tenant_id = i.tenant_id
WHERE i.tenant_id = $1
  AND (
    @search::text = ''
    OR cr.cnj_number ILIKE '%' || @search || '%' ESCAPE '\'
    OR cr.class ILIKE '%' || @search || '%'
    OR cr.judging_body ILIKE '%' || @search || '%'
  )
  AND (@type::text = '' OR i.type = @type::text)
  AND (@user_status::text = '' OR i.user_status = @user_status::text)
  AND (@court::text = '' OR cr.court = @court::text);

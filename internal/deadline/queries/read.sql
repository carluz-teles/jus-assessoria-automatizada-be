-- read-model queries (deadline slice) — the prazos SCREEN reads, kept OFF the write
-- path (docs: "leitura de tela usa read model, DTO por query dedicada"). Each is
-- tenant-scoped (barrier 1: an explicit tenant_id filter from the trusted principal,
-- never the body) and, where paginated, keyset-paginated on a stable (end_date, id)
-- pair — ascending, since a prazo agenda reads soonest-first. The caller passes a
-- sentinel cursor for the first page, so there is no conditional WHERE. days_left is
-- computed here as calendar days to end_date ((end_date - CURRENT_DATE)::int); the
-- urgency styling (gold < 3 dias) is the FE's call, not the read model's.

-- name: ListPrazosByProcesso :many
-- The Prazos tab of one process (GET /v1/processos/:id/prazos): the deadlines anchored
-- on the court_record, soonest vencimento first. @court_record_id is the court_record
-- id (the same id /processos returns). notification_id is the FK to intimation (the
-- historic column name, migration 0006) — the read model exposes it as intimation_id.
-- confirmed collapses confirmed_by IS NOT NULL to a bool (was the prazo human-approved).
SELECT d.id, d.kind, d.end_date,
       (d.end_date - CURRENT_DATE)::int AS days_left,
       d.counting, d.doubled, d.doubled_reason, d.status,
       d.holidays_applied, d.notification_id,
       (d.confirmed_by IS NOT NULL) AS confirmed
FROM deadline d
WHERE d.court_record_id = @court_record_id::uuid
  AND d.tenant_id = @tenant_id::uuid
  AND (d.end_date, d.id) > (@last_end::date, @last_id::uuid)
ORDER BY d.end_date ASC, d.id ASC
LIMIT @page_limit;

-- name: CountPrazosByProcesso :one
-- The "X de Y" total for the Prazos tab: how many prazos the process holds. Same
-- tenant + court_record scoping as the list.
SELECT count(*) FROM deadline d
WHERE d.court_record_id = @court_record_id::uuid
  AND d.tenant_id = @tenant_id::uuid;

-- name: ListPrazos :many
-- The global agenda (GET /v1/prazos): the tenant's prazos, soonest vencimento first,
-- with the process context (cnj_number/court) joined in. Optional filters: @status ('' =
-- all), @kind ('' = all), @court ('' = all), and an end_date window [@from_date, @to_date]
-- (NULL = open bound). Ascending keyset on (end_date, id); the first page passes the min
-- sentinel ('0001-01-01', zero-uuid).
SELECT d.id, d.kind, d.end_date,
       (d.end_date - CURRENT_DATE)::int AS days_left,
       d.counting, d.doubled, d.doubled_reason, d.status,
       d.holidays_applied, d.notification_id,
       (d.confirmed_by IS NOT NULL) AS confirmed,
       d.court_record_id, cr.cnj_number, cr.court
FROM deadline d
JOIN court_record cr ON cr.id = d.court_record_id
WHERE d.tenant_id = @tenant_id::uuid
  AND (@status::text = '' OR d.status = @status::text)
  AND (@kind::text = '' OR d.kind = @kind::text)
  AND (@court::text = '' OR cr.court = @court::text)
  AND (@from_date::date IS NULL OR d.end_date >= @from_date::date)
  AND (@to_date::date IS NULL OR d.end_date <= @to_date::date)
  AND (d.end_date, d.id) > (@last_end::date, @last_id::uuid)
ORDER BY d.end_date ASC, d.id ASC
LIMIT @page_limit;

-- name: ListPrazosByIntimacao :many
-- The prazo of ONE intimação (GET /v1/prazos?intimation_id=...): the F2 screen opens
-- from an intimação and needs its derived prazo. The deadline is 1:1 with the intimação
-- by notification_id (UNIQUE, migration 0006 column name), so this returns 0 or 1 rows.
-- SAME projection as ListPrazos (the agenda row shape, with the cnj/court context from
-- the join) so the handler can reuse the AgendaPrazoView envelope unchanged. Scoped to
-- tenant_id (barrier 1, from the principal — never the query): a foreign tenant sees no
-- prazo. No keyset/window filters here — the 1:1 lookup is already a single row.
SELECT d.id, d.kind, d.end_date,
       (d.end_date - CURRENT_DATE)::int AS days_left,
       d.counting, d.doubled, d.doubled_reason, d.status,
       d.holidays_applied, d.notification_id,
       (d.confirmed_by IS NOT NULL) AS confirmed,
       d.court_record_id, cr.cnj_number, cr.court
FROM deadline d
JOIN court_record cr ON cr.id = d.court_record_id
WHERE d.tenant_id = @tenant_id::uuid
  AND d.notification_id = @intimation_id::uuid
ORDER BY d.end_date ASC, d.id ASC;

-- name: CountPrazos :one
-- The filtered "X" of the agenda's "X de Y" counter: how many prazos match the active
-- @status / @kind / @court / end_date window. Called only when a filter is present; the
-- unfiltered "Y" reuses CountPrazosByTenant. The court filter needs the same court_record
-- join as the list, so it never multiplies rows (the join key is the record id).
SELECT count(*) FROM deadline d
JOIN court_record cr ON cr.id = d.court_record_id
WHERE d.tenant_id = @tenant_id::uuid
  AND (@status::text = '' OR d.status = @status::text)
  AND (@kind::text = '' OR d.kind = @kind::text)
  AND (@court::text = '' OR cr.court = @court::text)
  AND (@from_date::date IS NULL OR d.end_date >= @from_date::date)
  AND (@to_date::date IS NULL OR d.end_date <= @to_date::date);

-- name: CountPrazosByTenant :one
-- The tenant-wide "Y" of the agenda counter: every prazo the tenant holds, regardless
-- of any filter.
SELECT count(*) FROM deadline WHERE tenant_id = @tenant_id::uuid;

-- name: GetPrazo :one
-- The audit/detail view of one prazo (GET /v1/prazos/:id): every field the "por quê"
-- popover needs — the full holidays_applied, the rules_version that derived the days,
-- the origin intimation_id, and start/end/days/counting/doubled. Tenant-scoped (barrier
-- 1): a foreign id resolves to no row → pgx.ErrNoRows → typed ErrDeadlineNotFound (404)
-- at the repo, never (nil, nil).
SELECT d.id, d.court_record_id, d.kind, d.start_date, d.end_date,
       (d.end_date - CURRENT_DATE)::int AS days_left,
       d.days, d.counting, d.doubled, d.doubled_reason, d.status, d.source,
       d.holidays_applied, d.notification_id, d.rules_version,
       (d.confirmed_by IS NOT NULL) AS confirmed
FROM deadline d
WHERE d.id = @id::uuid AND d.tenant_id = @tenant_id::uuid;

-- name: GetPrazoSuggestContext :one
-- The advisory CASE CONTEXT for one prazo — the input the AI "intimação → tarefas sugeridas"
-- read (suggest.go) feeds the versioned meta-prompt with. This is NOT a screen DTO (it never
-- serializes to the FE): it is an internal read that gathers, in one tenant-scoped hop, the
-- prazo's own signals (kind/days/counting) PLUS the richer context the composer specializes on —
-- the process's court/degree/class/subject (court_record) and the origin intimação's type + teor
-- (intimation). deadline.notification_id is NOT NULL (every prazo is born from an intimação), so
-- both JOINs are inner. The teor is truncated to a bound so a long (often HTML) intimação never
-- blows the prompt or the transfer — LEFT counts characters, so the cut is rune-safe. Also carries
-- the PERSISTED ai_summary/ai_recommendation/ai_summary_generated_at (migration 0036, sync-on-
-- first-GET: NULL until the first successful LLM call, frozen thereafter) so the suggester can
-- serve the cached summary/recommendation instead of asking the model again. Tenant-scoped
-- (barrier 1): a foreign or unknown id yields no row → typed ErrDeadlineNotFound (404) at the
-- repo, never (nil, nil).
SELECT d.kind, d.days, d.counting, d.notification_id,
       cr.court, cr.degree, cr.class, cr.subject,
       i.type AS intimation_type,
       LEFT(i.content, 4000) AS intimation_text,
       d.ai_summary, d.ai_recommendation, d.ai_summary_generated_at
FROM deadline d
JOIN court_record cr ON cr.id = d.court_record_id
JOIN intimation i ON i.id = d.notification_id
WHERE d.id = @id::uuid AND d.tenant_id = @tenant_id::uuid;

-- ── task read models (GET /v1/processos/:id/tasks, GET /v1/tasks) ────────────
-- The task agenda reads soonest-due first, but due_date is NULLABLE (an undated backlog
-- task). Decisão travada: undated tasks sort LAST (a task with no date has no urgency, so it
-- trails the dated ones). We model NULLS LAST as a computed sort key
-- COALESCE(due_date, '9999-12-31') so the SAME ascending (sort_due, id) keyset the prazos
-- reads use works unchanged (a min sentinel '0001-01-01' for the first page; the handler
-- carries sort_due, not the raw due_date, into the next cursor). The read models expose the
-- REAL due_date (NULL for undated) — sort_due is a query-internal keyset column.

-- name: ListTasksByProcesso :many
-- The Tasks tab of one process (GET /v1/processos/:id/tasks): the tasks anchored on the
-- court_record, soonest due first (undated last). ALL statuses (the tab is not filtered — the
-- FE decides what to show). @court_record_id is the court_record id; an avulsa task (NULL
-- court_record_id) never matches, so it is absent from a process tab (correct — it hangs on no
-- process). Scoped to tenant_id (barrier 1).
SELECT t.id, t.title, t.description, t.kind, t.due_date,
       COALESCE(t.due_date, '9999-12-31')::date AS sort_due,
       t.status, t.source, t.assignee_user_id, t.deadline_id, t.intimation_id,
       t.court_record_id, t.completed_at,
       -- done_items feeds the derived display_status (Em execução once any item is ticked);
       -- the FE bucket reads it via display_status, not this raw count.
       COALESCE(p.done_items, 0)::bigint AS done_items
FROM task t
LEFT JOIN LATERAL (
  SELECT count(*) FILTER (WHERE ti.done) AS done_items
  FROM task_item ti
  WHERE ti.task_id = t.id AND ti.tenant_id = t.tenant_id
) p ON true
WHERE t.court_record_id = @court_record_id::uuid
  AND t.tenant_id = @tenant_id::uuid
  AND (COALESCE(t.due_date, '9999-12-31'), t.id) > (@last_due::date, @last_id::uuid)
ORDER BY COALESCE(t.due_date, '9999-12-31') ASC, t.id ASC
LIMIT @page_limit;

-- name: CountTasksByProcesso :one
-- The "X de Y" total for the Tasks tab: how many tasks the process holds. Same tenant +
-- court_record scoping as the list.
SELECT count(*) FROM task t
WHERE t.court_record_id = @court_record_id::uuid
  AND t.tenant_id = @tenant_id::uuid;

-- name: ListTasks :many
-- The global task agenda (GET /v1/tasks, "meus prazos"): the tenant's tasks, soonest due
-- first (undated last). Optional filters: @status ('' = all), @assignee_id (NULL = all
-- assignees; = principal.UserID for "meus"), @source ('' = all), and a due_date window
-- [@from_date, @to_date] (NULL = open bound). The window filters on the REAL due_date, so it
-- naturally EXCLUDES undated tasks (NULL >= date is NULL) — a dated-window query wants dated
-- items. Ascending (sort_due, id) keyset; the first page passes the min sentinel
-- ('0001-01-01', zero-uuid).
SELECT t.id, t.title, t.description, t.kind, t.due_date,
       COALESCE(t.due_date, '9999-12-31')::date AS sort_due,
       t.status, t.source, t.assignee_user_id, t.deadline_id, t.intimation_id,
       t.court_record_id, t.completed_at,
       -- done_items feeds the derived display_status (see ListTasksByProcesso).
       COALESCE(p.done_items, 0)::bigint AS done_items
FROM task t
LEFT JOIN LATERAL (
  SELECT count(*) FILTER (WHERE ti.done) AS done_items
  FROM task_item ti
  WHERE ti.task_id = t.id AND ti.tenant_id = t.tenant_id
) p ON true
WHERE t.tenant_id = @tenant_id::uuid
  AND (@status::text = '' OR t.status = @status::text)
  AND (sqlc.narg('assignee_id')::uuid IS NULL OR t.assignee_user_id = sqlc.narg('assignee_id')::uuid)
  AND (@source::text = '' OR t.source = @source::text)
  AND (@from_date::date IS NULL OR t.due_date >= @from_date::date)
  AND (@to_date::date IS NULL OR t.due_date <= @to_date::date)
  AND (COALESCE(t.due_date, '9999-12-31'), t.id) > (@last_due::date, @last_id::uuid)
ORDER BY COALESCE(t.due_date, '9999-12-31') ASC, t.id ASC
LIMIT @page_limit;

-- name: CountTasks :one
-- The filtered "X" of the task agenda's "X de Y" counter: how many tasks match the active
-- @status / @assignee_id / @source / window. Called only when a filter is present; the
-- unfiltered "Y" reuses CountTasksByTenant.
SELECT count(*) FROM task t
WHERE t.tenant_id = @tenant_id::uuid
  AND (@status::text = '' OR t.status = @status::text)
  AND (sqlc.narg('assignee_id')::uuid IS NULL OR t.assignee_user_id = sqlc.narg('assignee_id')::uuid)
  AND (@source::text = '' OR t.source = @source::text)
  AND (@from_date::date IS NULL OR t.due_date >= @from_date::date)
  AND (@to_date::date IS NULL OR t.due_date <= @to_date::date);

-- name: CountTasksByTenant :one
-- The tenant-wide "Y" of the task agenda counter: every task the tenant holds, regardless of
-- any filter.
SELECT count(*) FROM task WHERE tenant_id = @tenant_id::uuid;

-- ── task detail + checklist (GET /v1/tasks/:id) ─────────────────────────────
-- The task detail screen (docs/erd-prazos.md, a Tarefa aberta): the task's own fields + its
-- ordered checklist + the {done, total} progress the derived display_status reads. All
-- tenant-scoped (barrier 1). display_status is DERIVED in Go (read.go), not here — the SQL
-- returns the raw ingredients (status, due_date, and the progress counts).

-- name: GetTaskDetail :one
-- The task's own fields for the detail view, keyed by id and scoped to tenant_id (barrier 1).
-- A miss (foreign/unknown id) → pgx.ErrNoRows → typed ErrTaskNotFound (→ 404) at the mapper,
-- never (nil, nil). The checklist + progress are separate queries (a task with no items still
-- resolves). $1 = id, $2 = tenant_id.
SELECT t.id, t.title, t.description, t.kind, t.due_date,
       t.status, t.source, t.assignee_user_id, t.deadline_id, t.intimation_id,
       t.court_record_id, t.completed_at
FROM task t
WHERE t.id = @id::uuid AND t.tenant_id = @tenant_id::uuid;

-- name: ListTaskItems :many
-- One task's checklist, ordered by position (the detail view). Scoped to (task_id, tenant_id)
-- (barrier 1). An empty checklist is 0 rows (not an error). $1 = task_id, $2 = tenant_id.
SELECT id, task_id, title, position, done, done_at, created_at
FROM task_item
WHERE task_id = @task_id::uuid AND tenant_id = @tenant_id::uuid
ORDER BY position ASC, id ASC;

-- name: GetTaskItemProgress :one
-- The {done, total} progress of one task's checklist (the detail view's progress bar + the
-- Em execução signal for display_status). Scoped to (task_id, tenant_id) (barrier 1). An empty
-- checklist yields {0, 0}. $1 = task_id, $2 = tenant_id.
SELECT count(*) FILTER (WHERE done)::bigint AS done,
       count(*)::bigint                     AS total
FROM task_item
WHERE task_id = @task_id::uuid AND tenant_id = @tenant_id::uuid;

-- ── KPI summaries (GET /v1/prazos/summary, GET /v1/tasks/summary) ────────────
-- Single-object read models for the Tarefas/Prazos cockpit KPIs, aggregated per tenant. Both
-- follow the intimacoes/summary precedent (0030): one query, count(*) FILTER per bucket, no
-- pagination. The buckets' semantics are documented at the read.go DTOs.

-- name: GetPrazosSummary :one
-- The prazos KPI counts, derived from deadline.status + days_left ((end_date - CURRENT_DATE)),
-- scoped to tenant_id (barrier 1). The buckets (thresholds documented at PrazosSummary in
-- read.go): criticos = OPEN/PENDING with days_left <= 1; vencendo = OPEN/PENDING with days_left
-- in 0..3; abertos = every OPEN/PENDING; futuros = OPEN/PENDING starting in the future
-- (start_date > today); vencidos = MISSED or an OPEN/PENDING already past (days_left < 0);
-- cumpridos = MET. CANCELLED is counted only in total. $1 = tenant_id.
SELECT
  count(*)::bigint AS total,
  count(*) FILTER (
    WHERE d.status IN ('OPEN', 'PENDING') AND (d.end_date - CURRENT_DATE) <= 1
  )::bigint AS criticos,
  count(*) FILTER (
    WHERE d.status IN ('OPEN', 'PENDING') AND (d.end_date - CURRENT_DATE) BETWEEN 0 AND 3
  )::bigint AS vencendo,
  count(*) FILTER (WHERE d.status IN ('OPEN', 'PENDING'))::bigint AS abertos,
  count(*) FILTER (
    WHERE d.status IN ('OPEN', 'PENDING') AND d.start_date > CURRENT_DATE
  )::bigint AS futuros,
  count(*) FILTER (
    WHERE d.status = 'MISSED'
       OR (d.status IN ('OPEN', 'PENDING') AND (d.end_date - CURRENT_DATE) < 0)
  )::bigint AS vencidos,
  count(*) FILTER (WHERE d.status = 'MET')::bigint AS cumpridos
FROM deadline d
WHERE d.tenant_id = @tenant_id::uuid;

-- name: GetTasksSummary :one
-- The tasks KPI counts, derived from the SAME display_status logic the detail/list views use,
-- scoped to tenant_id (barrier 1). em_execucao needs the checklist progress, so a LEFT JOIN
-- LATERAL folds each task's done-item count in. Buckets (DISMISSED excluded, mirrors the derived
-- display_status): concluidas = DONE; atrasadas = OPEN with due_date < today; em_execucao = OPEN,
-- not yet due, with at least one done item; abertas = OPEN, not yet due, no done item. $1 = tenant_id.
SELECT
  count(*) FILTER (WHERE t.status = 'DONE')::bigint AS concluidas,
  count(*) FILTER (
    WHERE t.status = 'OPEN' AND t.due_date IS NOT NULL AND t.due_date < CURRENT_DATE
  )::bigint AS atrasadas,
  count(*) FILTER (
    WHERE t.status = 'OPEN'
      AND NOT (t.due_date IS NOT NULL AND t.due_date < CURRENT_DATE)
      AND p.done_items > 0
  )::bigint AS em_execucao,
  count(*) FILTER (
    WHERE t.status = 'OPEN'
      AND NOT (t.due_date IS NOT NULL AND t.due_date < CURRENT_DATE)
      AND p.done_items = 0
  )::bigint AS abertas
FROM task t
LEFT JOIN LATERAL (
  SELECT count(*) FILTER (WHERE ti.done) AS done_items
  FROM task_item ti
  WHERE ti.task_id = t.id AND ti.tenant_id = t.tenant_id
) p ON true
WHERE t.tenant_id = @tenant_id::uuid;

-- ── filter options (the envelope's selectable sets) ──────────────────────────
-- Distinct-value reads that back the prazos/tasks agenda envelopes' filter chips.
-- Each is tenant-scoped (barrier 1) and mirrors the list's own predicates, so the
-- options reflect what the list can actually show. Empty values are skipped in Go.

-- name: ListPrazoKinds :many
-- Selectable ?kind values for the prazos agenda: the distinct kinds of the tenant's
-- prazos, ordered by name.
SELECT DISTINCT d.kind
FROM deadline d
WHERE d.tenant_id = @tenant_id::uuid
  AND d.kind <> ''
ORDER BY LOWER(d.kind) ASC;

-- name: ListPrazoCourts :many
-- Selectable ?court values for the prazos agenda: the distinct courts of the tenant's
-- intimated court records (the same join the list uses), ordered by name.
SELECT DISTINCT cr.court
FROM deadline d
JOIN court_record cr ON cr.id = d.court_record_id
WHERE d.tenant_id = @tenant_id::uuid
  AND cr.court <> ''
ORDER BY LOWER(cr.court) ASC;

-- name: ListTaskAssignees :many
-- Selectable ?assignee values for the task agenda ("meus prazos"): the distinct
-- responsáveis of the tenant's tasks, deduped by id, ordered by name. The LEFT JOIN
-- app_user resolves a name when the id is a known user (the column is a bare uuid with
-- no FK); an unknown id yields an empty name — the FE labels it "ID sem nome".
SELECT DISTINCT t.assignee_user_id, COALESCE(au.name, '') AS name
FROM task t
LEFT JOIN app_user au ON au.id = t.assignee_user_id
WHERE t.tenant_id = @tenant_id::uuid
  AND t.assignee_user_id IS NOT NULL
ORDER BY LOWER(COALESCE(au.name, '')) ASC;

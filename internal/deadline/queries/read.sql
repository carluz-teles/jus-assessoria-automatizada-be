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
-- all) and an end_date window [@from_date, @to_date] (NULL = open bound). Ascending
-- keyset on (end_date, id); the first page passes the min sentinel ('0001-01-01',
-- zero-uuid).
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
-- @status / end_date window. Called only when a filter is present; the unfiltered "Y"
-- reuses CountPrazosByTenant.
SELECT count(*) FROM deadline d
WHERE d.tenant_id = @tenant_id::uuid
  AND (@status::text = '' OR d.status = @status::text)
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
       t.court_record_id, t.completed_at
FROM task t
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
-- assignees; = principal.UserID for "meus"), and a due_date window [@from_date, @to_date]
-- (NULL = open bound). The window filters on the REAL due_date, so it naturally EXCLUDES
-- undated tasks (NULL >= date is NULL) — a dated-window query wants dated items. Ascending
-- (sort_due, id) keyset; the first page passes the min sentinel ('0001-01-01', zero-uuid).
SELECT t.id, t.title, t.description, t.kind, t.due_date,
       COALESCE(t.due_date, '9999-12-31')::date AS sort_due,
       t.status, t.source, t.assignee_user_id, t.deadline_id, t.intimation_id,
       t.court_record_id, t.completed_at
FROM task t
WHERE t.tenant_id = @tenant_id::uuid
  AND (@status::text = '' OR t.status = @status::text)
  AND (sqlc.narg('assignee_id')::uuid IS NULL OR t.assignee_user_id = sqlc.narg('assignee_id')::uuid)
  AND (@from_date::date IS NULL OR t.due_date >= @from_date::date)
  AND (@to_date::date IS NULL OR t.due_date <= @to_date::date)
  AND (COALESCE(t.due_date, '9999-12-31'), t.id) > (@last_due::date, @last_id::uuid)
ORDER BY COALESCE(t.due_date, '9999-12-31') ASC, t.id ASC
LIMIT @page_limit;

-- name: CountTasks :one
-- The filtered "X" of the task agenda's "X de Y" counter: how many tasks match the active
-- @status / @assignee_id / window. Called only when a filter is present; the unfiltered "Y"
-- reuses CountTasksByTenant.
SELECT count(*) FROM task t
WHERE t.tenant_id = @tenant_id::uuid
  AND (@status::text = '' OR t.status = @status::text)
  AND (sqlc.narg('assignee_id')::uuid IS NULL OR t.assignee_user_id = sqlc.narg('assignee_id')::uuid)
  AND (@from_date::date IS NULL OR t.due_date >= @from_date::date)
  AND (@to_date::date IS NULL OR t.due_date <= @to_date::date);

-- name: CountTasksByTenant :one
-- The tenant-wide "Y" of the task agenda counter: every task the tenant holds, regardless of
-- any filter.
SELECT count(*) FROM task WHERE tenant_id = @tenant_id::uuid;

-- deadline slice queries (the prazos CREATION path). Every write and read runs inside
-- the use case's transaction so RLS scopes it to the event's tenant (barrier 2) on top
-- of the explicit tenant filter (barrier 1). Absence is a typed error at the mapper,
-- never (nil, nil).

-- name: GetCourtRecordClass :one
-- Read the rito signal (class) for the record the intimação hangs on — the one
-- cross-table read the derivation needs (decisão P1: read the table, never import the
-- acquisition package). A missing record → pgx.ErrNoRows → typed not-found at the
-- mapper. class itself may be NULL (mapped to "").
-- tenant_id is filtered explicitly (barrier 1) even though RLS (barrier 2) already
-- scopes the tx: a NULL app.tenant_id or an RLS regression would otherwise let a read
-- cross tenants. $2 = tenant_id, from the trusted event payload (never the body).
SELECT class
FROM court_record
WHERE id = $1 AND tenant_id = $2;

-- name: ResolveDeadlineRule :one
-- Resolve the conservative rule for (intimation_type, court) in a rules version. The
-- resolution lives HERE, in SQL (decisão travada, erd-prazos.md §8/§11): the most
-- SPECIFIC active match wins, falling back to the '*' catch-all — so an unknown type
-- still yields the safe GENERICO rule. Ordering, most specific first:
--   1. an exact intimation_type beats the '*' catch-all;
--   2. a court-specific rule (court_prefix set) beats an any-court rule;
--   3. a longer court_prefix is more specific than a shorter one;
--   4. finally, higher priority wins.
-- $1 = rules_version, $2 = intimation_type, $3 = court. A court_prefix matches when the
-- court sigla starts with it ($3 LIKE prefix || '%'); a NULL prefix matches any court.
SELECT rules_version, kind, days, counting, doubled
FROM deadline_rule
WHERE rules_version = $1
  AND active
  AND (intimation_type = $2 OR intimation_type = '*')
  AND (court_prefix IS NULL OR $3 LIKE court_prefix || '%')
ORDER BY (intimation_type = $2) DESC,
         (court_prefix IS NOT NULL) DESC,
         length(coalesce(court_prefix, '')) DESC,
         priority DESC
LIMIT 1;

-- name: InsertDeadline :one
-- Persist the derived prazo, BORN PENDING (status), source RULE. Idempotent on the 1:1
-- notification_id (UNIQUE): ON CONFLICT DO NOTHING yields NO row on a re-derivation, so
-- the mapper reads pgx.ErrNoRows as "already exists" (ErrDeadlineExists) instead of
-- poisoning the tx with a constraint error. confirmed_by/at stay NULL (no human aval
-- yet — that is the F2 slice). Returns the DB-assigned id; the repo maps the rest from
-- the input entity.
INSERT INTO deadline (
    tenant_id, court_record_id, notification_id,
    start_date, end_date, days, counting, doubled, doubled_reason,
    holidays_applied, status, source, kind, rules_version
) VALUES (
    $1, $2, $3,
    $4, $5, $6, $7, $8, $9,
    $10, $11, $12, $13, $14
)
ON CONFLICT (notification_id) DO NOTHING
RETURNING id;

-- name: GetDeadlineForConfirm :one
-- Load a PENDING prazo's confirmation anchor — the F2 "Aprovar tudo" (§9) reads it
-- BEFORE the recompute: start_date is the fixed anchor of the calendar math (=
-- intimation.deadline_start_at, already persisted at derivation), court_record_id feeds
-- both the court lookup (for the recompute UF) and the inserted tasks. Keyed by the 1:1
-- notification_id (=intimation id) and scoped to tenant_id (barrier 1, on top of RLS
-- barrier 2). A missing prazo → pgx.ErrNoRows → typed ErrDeadlineNotFound at the mapper,
-- never (nil, nil): confirming an intimação with no derived prazo is a 404 at the edge.
-- $1 = intimation_id (the notification_id column), $2 = tenant_id (from the principal).
SELECT id, court_record_id, start_date
FROM deadline
WHERE notification_id = $1 AND tenant_id = $2;

-- name: GetCourtRecordCourt :one
-- Read the court sigla for the record the prazo hangs on — the confirmation recompute
-- derives the UF from it (pkg/tribunal.UF) for the state-holiday calendar lookup, the
-- confirm-path counterpart of GetCourtRecordClass. Scoped to tenant_id (barrier 1). A
-- missing record → pgx.ErrNoRows → typed not-found at the mapper. court is NOT NULL.
-- $1 = id, $2 = tenant_id, both from the trusted principal's request context.
SELECT court
FROM court_record
WHERE id = $1 AND tenant_id = $2;

-- name: ConfirmDeadline :one
-- The F2 confirmation (§9: "Aprovar tudo" grava o deadline PENDING→OPEN recalculado).
-- Flips the prazo to OPEN with the human-approved {kind, days, counting, doubled,
-- doubled_reason} and the RECOMPUTED {end_date, holidays_applied}, stamping who/when
-- (confirmed_by/at). Keyed by the 1:1 notification_id and scoped to tenant_id (barrier
-- 1). IDEMPOTENT on the deadline: re-confirming the same intimação re-UPDATEs the one row
-- (the 1:1 notification_id) — it never opens a second prazo. source/start_date/
-- rules_version are LEFT AS-IS: source keeps its provenance (RULE/AI), start_date is the
-- fixed anchor, and rules_version still records which rule set first derived the prazo
-- even when the human overrode the days. A no-match (no prazo for the intimação) yields
-- NO row → pgx.ErrNoRows → ErrDeadlineNotFound at the mapper. $1 = intimation_id, $2 =
-- tenant_id, then the confirmed fields.
UPDATE deadline
SET status           = 'OPEN',
    kind             = $3,
    days             = $4,
    counting         = $5,
    doubled          = $6,
    doubled_reason   = $7,
    end_date         = $8,
    holidays_applied = $9,
    confirmed_by     = $10,
    confirmed_at     = $11
WHERE notification_id = $1 AND tenant_id = $2
RETURNING id, court_record_id;

-- name: InsertTask :one
-- Persist one F2 action item (§4: 1 legal prazo → N tasks, gravadas na MESMA tx do
-- confirm). Born status='OPEN', source='MANUAL' (a human created it at confirmation —
-- AI-suggested tasks are a later slice). All FKs but tenant_id are nullable per the 0024
-- schema, but confirm always fills court_record_id/deadline_id/intimation_id/created_by
-- (the prazo's context); assignee_user_id/due_date/description/kind are optional. Returns
-- the DB-assigned id so task.created commits with it in the SAME tx. $1.. are the columns.
INSERT INTO task (
    tenant_id, court_record_id, deadline_id, intimation_id,
    title, description, kind, due_date, status, source, assignee_user_id, created_by
) VALUES (
    $1, $2, $3, $4,
    $5, $6, $7, $8, $9, $10, $11, $12
)
RETURNING id;

-- name: GetTaskForUpdate :one
-- Load a task's editable state — the manual ajuste (§9: PATCH /v1/tasks/:id) reads it BEFORE
-- the merge so an absent body field keeps its stored value (a partial patch, not a full
-- replace). Keyed by id and scoped to tenant_id (barrier 1, on top of RLS barrier 2). A
-- missing id in the tenant → pgx.ErrNoRows → typed ErrTaskNotFound at the mapper (→ 404),
-- never (nil, nil). $1 = id, $2 = tenant_id (from the principal). status is carried for the
-- caller even though PATCH never changes it (edit is orthogonal to the lifecycle).
SELECT id, status, title, description, kind, due_date, assignee_user_id
FROM task
WHERE id = $1 AND tenant_id = $2;

-- name: UpdateTask :one
-- Manual ajuste of a task (§9: PATCH /v1/tasks/:id → editar tarefa). Writes the merged
-- {title, description, kind, due_date, assignee_user_id} (already merged over the stored
-- values by the use case), keyed by id and scoped to tenant_id (barrier 1). status/source/
-- created_by/completed_at and the context FKs are LEFT AS-IS: the edit never changes the
-- lifecycle nor re-parents the task. A no-match (the row vanished mid-tx) → pgx.ErrNoRows →
-- ErrTaskNotFound at the mapper. Returns the full task row so the response renders without a
-- re-read. $1 = id, $2 = tenant_id, then the patched fields.
UPDATE task
SET title            = $3,
    description      = $4,
    kind             = $5,
    due_date         = $6,
    assignee_user_id = $7
WHERE id = $1 AND tenant_id = $2
RETURNING id, tenant_id, court_record_id, deadline_id, intimation_id,
          title, description, kind, due_date, status, source, assignee_user_id,
          created_by, completed_at;

-- name: GetTaskForTransition :one
-- Re-read a task's CURRENT status before a manual transition (§9: POST /v1/tasks/:id/done |
-- .../dismiss). Keyed by id and scoped to tenant_id (barrier 1). The use case pre-checks the
-- transition on this status so it can distinguish a 404 miss (ErrTaskNotFound) from a 409
-- invalid transition (ErrTaskNotOpen), mirroring the deadline met/missed path. A missing id in
-- the tenant → pgx.ErrNoRows → typed ErrTaskNotFound at the mapper. $1 = id, $2 = tenant_id.
SELECT id, status
FROM task
WHERE id = $1 AND tenant_id = $2;

-- name: MarkTaskStatus :one
-- Manual lifecycle transition of a task (§9: POST /v1/tasks/:id/done → OPEN→DONE stamping
-- completed_at; .../dismiss → OPEN→DISMISSED, completed_at NULL). Flips status from
-- current_status to new_status and sets completed_at (the caller passes now() for done, NULL
-- for dismiss), keyed by id and scoped to tenant_id (barrier 1). The `status = current_status`
-- guard makes the flip SAFE and IDEMPOTENT under concurrency — the caller pre-checks the
-- transition, and this guard defends the write against a racing flip; a no-match (already
-- transitioned) → pgx.ErrNoRows → typed not-found at the mapper. On a hit it returns the id so
-- task.completed/task.dismissed commits in the SAME tx.
UPDATE task
SET status = sqlc.arg(new_status), completed_at = sqlc.arg(completed_at)
WHERE id = sqlc.arg(id) AND tenant_id = sqlc.arg(tenant_id) AND status = sqlc.arg(current_status)
RETURNING id;

-- name: ListTaskTitlesByDeadline :many
-- List the titles of the tasks that CURRENTLY exist for a confirmed prazo, by deadline_id and
-- scoped to tenant_id (barrier 1, on top of RLS barrier 2). The F2 confirm reads it INSIDE its
-- tx to measure the AI-suggestion delta (feedback loop, camada 2) against the tasks that REALLY
-- exist — the Análise section creates them via POST /v1/tasks, so the confirm no longer owns the
-- task lifecycle and must not diff against a submitted set. Only the title is read (the delta
-- keys off title alone). No rows → an empty slice (a prazo confirmed before any task was created),
-- never an error. ORDER BY created_at keeps the result deterministic. $1 = deadline_id, $2 =
-- tenant_id, both from the confirm tx (the id ConfirmDeadline returned + the principal).
SELECT title
FROM task
WHERE deadline_id = $1 AND tenant_id = $2
ORDER BY created_at;

-- ── task_item write path (checklist / subtarefas, §4/§10) ────────────────────
-- The Tarefas screen's checklist: a task grows N ordered items the user ticks off. Every
-- write is keyed by the parent task and scoped to tenant_id (barrier 1, on top of RLS
-- barrier 2). The parent-task guard (TaskExistsInTenant) turns a foreign/unknown task_id
-- into a typed 404 BEFORE any item write, so an item can never be grafted onto another
-- tenant's task.

-- name: TaskExistsInTenant :one
-- Guard the item writes: does this task exist in the tenant? POST /v1/tasks/:id/items
-- checks it BEFORE inserting so a foreign/unknown :id is a typed ErrTaskNotFound (→ 404) at
-- the mapper, not a phantom item on nothing (or a raw FK error). $1 = id, $2 = tenant_id.
SELECT id
FROM task
WHERE id = $1 AND tenant_id = $2;

-- name: NextTaskItemPosition :one
-- The position a newly appended checklist item takes: one past the current max within the
-- task (0 when the checklist is empty). Scoped to tenant_id (barrier 1). Keeps positions
-- gap-free-ish on append (the FE may later rewrite them to reorder). $1 = task_id, $2 =
-- tenant_id.
SELECT COALESCE(MAX(position) + 1, 0)::int AS next_position
FROM task_item
WHERE task_id = $1 AND tenant_id = $2;

-- name: InsertTaskItem :one
-- Append one checklist item to a task (POST /v1/tasks/:id/items). Born done=false (the
-- default), done_at NULL. tenant_id/task_id come from the request context + the guarded
-- parent; position is the computed append slot. Returns the whole row so the handler renders
-- it without a re-read. $1 = tenant_id, $2 = task_id, $3 = title, $4 = position.
INSERT INTO task_item (tenant_id, task_id, title, position)
VALUES ($1, $2, $3, $4)
RETURNING id, task_id, title, position, done, done_at, created_at;

-- name: GetTaskItemForUpdate :one
-- Load a checklist item's current {title, done} before the partial PATCH (PATCH
-- /v1/tasks/:id/items/:itemId), keyed by (item id, parent task id) and scoped to tenant_id
-- (barrier 1). Binding task_id too means an itemId under a DIFFERENT task is a miss (→ 404),
-- not a cross-task edit. A missing row → pgx.ErrNoRows → typed ErrTaskItemNotFound at the
-- mapper. $1 = id, $2 = task_id, $3 = tenant_id.
SELECT id, task_id, title, done
FROM task_item
WHERE id = $1 AND task_id = $2 AND tenant_id = $3;

-- name: UpdateTaskItem :one
-- Write the merged {title, done, done_at} of a checklist item (PATCH …/items/:itemId),
-- keyed by (id, task_id, tenant_id) (barrier 1). done_at is set/cleared by the use case to
-- match done (a real time when done flips true, NULL when it flips false). A no-match → 404
-- at the mapper. Returns the whole row so the handler renders it without a re-read. $1 = id,
-- $2 = task_id, $3 = tenant_id, $4 = title, $5 = done, $6 = done_at.
UPDATE task_item
SET title   = $4,
    done    = $5,
    done_at = $6
WHERE id = $1 AND task_id = $2 AND tenant_id = $3
RETURNING id, task_id, title, position, done, done_at, created_at;

-- name: DeleteTaskItem :one
-- Remove one checklist item (DELETE …/items/:itemId), keyed by (id, task_id, tenant_id)
-- (barrier 1). Returns the deleted id so a no-match (foreign/unknown item) → pgx.ErrNoRows →
-- typed ErrTaskItemNotFound at the mapper (→ 404), never a silent 204 on nothing. $1 = id,
-- $2 = task_id, $3 = tenant_id.
DELETE FROM task_item
WHERE id = $1 AND task_id = $2 AND tenant_id = $3
RETURNING id;

-- name: GetDeadlineForAdjust :one
-- Load a prazo's FULL adjustable state — the F2 ajuste manual (§9: PATCH /v1/prazos/:id)
-- reads it BEFORE the recompute: start_date is the fixed anchor the calendar re-counts
-- from, court_record_id feeds the court lookup (recompute UF), and the CURRENT
-- {kind, days, counting, doubled, doubled_reason} are the base the partial patch is applied
-- over (a field absent from the body keeps its stored value). status gates the ajuste (only
-- a PENDING/OPEN prazo is adjustable). Keyed by id and scoped to tenant_id (barrier 1, on top
-- of RLS barrier 2). A missing id in the tenant → pgx.ErrNoRows → typed ErrDeadlineNotFound at
-- the mapper (→ 404), never (nil, nil). $1 = id, $2 = tenant_id (from the principal).
SELECT id, court_record_id, start_date, status, kind, days, counting, doubled, doubled_reason
FROM deadline
WHERE id = $1 AND tenant_id = $2;

-- name: UpdateDeadlineAdjust :one
-- Ajuste manual do prazo legal (§9: PATCH /v1/prazos/:id → recalcula datas). Writes the
-- patched {kind, days, counting, doubled, doubled_reason} and the RECOMPUTED {end_date,
-- holidays_applied} (from the fixed start_date), keyed by id and scoped to tenant_id (barrier
-- 1). status is LEFT AS-IS: the ajuste never changes the lifecycle (a PENDING stays PENDING, an
-- OPEN stays OPEN — the use case already refused a terminal prazo); source/start_date/
-- rules_version/confirmed_* are untouched (the anchor and provenance persist). A no-match (the
-- row vanished mid-tx) → pgx.ErrNoRows → ErrDeadlineNotFound at the mapper. Returns the prazo id
-- and the record it hangs on. $1 = id, $2 = tenant_id, then the patched fields.
UPDATE deadline
SET kind             = $3,
    days             = $4,
    counting         = $5,
    doubled          = $6,
    doubled_reason   = $7,
    end_date         = $8,
    holidays_applied = $9
WHERE id = $1 AND tenant_id = $2
RETURNING id, court_record_id;

-- name: MarkDeadlineStatus :one
-- Manual lifecycle transition of a prazo (§9: POST /v1/prazos/:id/met | .../missed → marca
-- cumprido/perdido). Flips status from current_status to new_status, keyed by id and scoped
-- to tenant_id (barrier 1). The `status = current_status` guard makes the flip SAFE and
-- IDEMPOTENT under concurrency: the caller pre-checks the transition (loading the prazo for a
-- distinct not-found vs invalid-transition error), and this guard defends the write against a
-- racing flip — a no-match (already transitioned) → pgx.ErrNoRows → typed not-found at the
-- mapper. On a hit it returns the id so deadline.met/deadline.missed commits in the SAME tx.
UPDATE deadline
SET status = sqlc.arg(new_status)
WHERE id = sqlc.arg(id) AND tenant_id = sqlc.arg(tenant_id) AND status = sqlc.arg(current_status)
RETURNING id;

-- name: GetDeadlineForCheck :one
-- Re-read a prazo at a scheduled mark's fire time (deadline.reminder_check): the CURRENT
-- status the fire handler branches on, plus the end_date and the context (kind, counting,
-- court_record_id) a lembrete or MISSED fact may carry. Keyed by id and scoped to tenant_id
-- (barrier 1, on top of RLS barrier 2). A missing id in the tenant → pgx.ErrNoRows → typed
-- ErrDeadlineNotFound at the mapper, never (nil, nil). $1 = id, $2 = tenant_id, both from
-- the trusted scheduled-event payload.
SELECT id, status, end_date, court_record_id, kind, counting
FROM deadline
WHERE id = $1 AND tenant_id = $2;

-- name: MarkMissed :one
-- Auto-mark a prazo MISSED at the D+1 carência (deadline.missed_check fire path). Scoped to
-- tenant_id (barrier 1). The `status = 'OPEN' AND end_date < CURRENT_DATE` guard makes the
-- flip SAFE and IDEMPOTENT (decisão travada: MISSED auto D+1 SÓ em OPEN — nunca perder um
-- PENDING não confirmado): a redelivery, a PENDING/terminal prazo, or one not yet overdue
-- updates NO row → pgx.ErrNoRows → typed not-found at the mapper, the use case's no-op
-- (never a phantom deadline.missed). On a hit it returns the id so deadline.missed commits
-- in the SAME tx. $1 = id, $2 = tenant_id, both from the trusted scheduled-event payload.
UPDATE deadline
SET status = 'MISSED'
WHERE id = $1 AND tenant_id = $2 AND status = 'OPEN' AND end_date < CURRENT_DATE
RETURNING id;

-- name: RevokeDeadlineByIntimation :one
-- Revoke (CANCEL) the prazo derived from an intimação the DJEN retracted (erd-prazos.md
-- §7/§11: uma intimação retificada vira prazo-fantasma → deadline.revoked). Keyed by the
-- 1:1 notification_id (=intimation id) and scoped to tenant_id (barrier 1, on top of RLS
-- barrier 2). The `status <> 'CANCELLED'` guard makes the revoke IDEMPOTENT: a redelivery
-- past the dedup — or a cancel that lands before any prazo exists, or on an already
-- CANCELLED one — updates NO row → pgx.ErrNoRows → typed not-found at the mapper, the use
-- case's safe no-op (never a phantom revoked). On a hit it returns the revoked id (+ the
-- record it hung on) so deadline.revoked commits in the SAME tx. $1 = intimation_id (the
-- notification_id column), $2 = tenant_id, both from the trusted event payload.
UPDATE deadline
SET status = 'CANCELLED'
WHERE notification_id = $1 AND tenant_id = $2 AND status <> 'CANCELLED'
RETURNING id, court_record_id;

-- name: InsertTaskSuggestion :one
-- Grava a proveniência de UMA rodada de sugestão da IA (feedback loop, camada 1). Chamada
-- no caminho de LEITURA (GET /v1/prazos/:id/suggested-tasks), logo após o LLM devolver as
-- tarefas: registra COM QUE prompt_version e COM QUE model foram geradas, e o payload exato
-- (suggested jsonb: [{title, kind}, …]). É best-effort — uma falha aqui não quebra o F2.
-- Retorna o id para log/telemetria. $1..$6 são as colunas na ordem do INSERT.
INSERT INTO task_suggestion (
    tenant_id, deadline_id, intimation_id, prompt_version, model, suggested
) VALUES (
    $1, $2, $3, $4, $5, $6
)
RETURNING id;

-- name: GetLatestTaskSuggestion :one
-- Lê a sugestão MAIS RECENTE de um prazo, pela intimação 1:1 e escopada por tenant_id
-- (barrier 1). O confirm (F2 "Aprovar tudo") a carrega para medir o DELTA entre o que a IA
-- sugeriu e o que o humano confirmou. Sem sugestão (prazo manual, IA nunca usada) NÃO há
-- linha → pgx.ErrNoRows → o use case trata como "sem feedback" (não emite evento). $1 =
-- intimation_id, $2 = tenant_id.
SELECT id, prompt_version, model, suggested
FROM task_suggestion
WHERE intimation_id = $1 AND tenant_id = $2
ORDER BY created_at DESC
LIMIT 1;

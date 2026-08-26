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
SELECT rules_version, kind, days, counting, doubled, legal_citation
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
    holidays_applied, status, source, kind, rules_version,
    anchor_event, legal_citation
) VALUES (
    $1, $2, $3,
    $4, $5, $6, $7, $8, $9,
    $10, $11, $12, $13, $14,
    $15, $16
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
SELECT id, court_record_id, start_date, legal_citation
FROM deadline
WHERE notification_id = $1 AND tenant_id = $2;

-- name: GetPreviewContext :one
-- Load the confirmation panel's PREVIEW context for an intimação (POST /v1/prazos/preview,
-- read-only, off the transactional path): the three observed anchor dates PLUS the court sigla
-- of the process the intimação hangs on (for the recompute's state-holiday UF). One tenant-scoped
-- hop (barrier 1). deadline.notification_id is not involved — the preview keys directly on the
-- intimation id (the FE has it from the intimação detail). A missing/foreign id → pgx.ErrNoRows →
-- typed ErrDeadlineNotFound at the mapper, never (nil, nil). $1 = intimation_id, $2 = tenant_id.
SELECT i.made_available_at, i.published_at, i.deadline_start_at, cr.court
FROM intimation i
JOIN court_record cr ON cr.id = i.court_record_id
WHERE i.id = $1 AND i.tenant_id = $2;

-- name: GetIntimationAnchors :one
-- Load the three observed dates of the intimação the confirmation panel can re-anchor a prazo
-- on (§3 "termo inicial"): made_available_at (disponibilização), published_at (publicação) and
-- deadline_start_at (início da contagem — the legacy anchor). All three are NOT NULL date
-- columns, so a present intimação always yields three real dates. Keyed by the intimation id and
-- scoped to tenant_id (barrier 1, on top of RLS barrier 2). A missing/foreign id → pgx.ErrNoRows
-- → typed ErrDeadlineNotFound at the mapper (the prazo's anchor cannot be resolved), never
-- (nil, nil). $1 = intimation_id, $2 = tenant_id, both from the trusted principal's context.
SELECT made_available_at, published_at, deadline_start_at
FROM intimation
WHERE id = $1 AND tenant_id = $2;

-- name: GetIntimationAssignee :one
-- Lê SÓ o assignee_user_id vigente da intimação, dentro da tx do caller, scoped a
-- tenant_id. POST /v1/tasks usa para herdar o responsável no momento da criação
-- (snapshot, não vínculo contínuo) quando intimation_id vem preenchido e
-- assignee_user_id vem vazio. Lê a tabela intimation diretamente — sem importar
-- internal/acquisition (mesmo precedente de GetIntimationAnchors). assignee_user_id
-- pode ser NULL (intimação sem responsável) → nil, sem erro.
SELECT assignee_user_id
FROM intimation
WHERE id = @intimation_id::uuid AND tenant_id = @tenant_id::uuid;

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
-- (the 1:1 notification_id) — it never opens a second prazo. source/rules_version are LEFT
-- AS-IS: source keeps its provenance (RULE/AI) and rules_version still records which rule set
-- first derived the prazo even when the human overrode the days. start_date IS written now: the
-- confirmation panel may re-anchor (anchor_event → a different intimação date), and
-- anchor_event/manual_extra_days/legal_citation persist the panel's choices. A no-match yields
-- NO row → pgx.ErrNoRows → ErrDeadlineNotFound at the mapper. $1 = intimation_id, $2 =
-- tenant_id, then the confirmed fields.
UPDATE deadline
SET status            = 'OPEN',
    kind              = $3,
    days              = $4,
    counting          = $5,
    doubled           = $6,
    doubled_reason    = $7,
    end_date          = $8,
    holidays_applied  = $9,
    confirmed_by      = $10,
    confirmed_at      = $11,
    start_date        = sqlc.arg(start_date),
    anchor_event      = sqlc.arg(anchor_event),
    manual_extra_days = sqlc.arg(manual_extra_days),
    legal_citation    = sqlc.arg(legal_citation)
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
    title, description, kind, priority, due_date, status, source, assignee_user_id, created_by
) VALUES (
    $1, $2, $3, $4,
    $5, $6, $7, $8, $9, $10, $11, $12, $13
)
RETURNING id;

-- name: GetTaskForUpdate :one
-- Load a task's editable state — the manual ajuste (§9: PATCH /v1/tasks/:id) reads it BEFORE
-- the merge so an absent body field keeps its stored value (a partial patch, not a full
-- replace). Keyed by id and scoped to tenant_id (barrier 1, on top of RLS barrier 2). A
-- missing id in the tenant → pgx.ErrNoRows → typed ErrTaskNotFound at the mapper (→ 404),
-- never (nil, nil). $1 = id, $2 = tenant_id (from the principal). status is carried for the
-- caller even though PATCH never changes it (edit is orthogonal to the lifecycle). deadline_id
-- is carried so the edit can enforce ERD §4's due_date ≤ end_date invariant when it touches
-- the task's own date.
SELECT id, status, title, description, kind, priority, due_date, assignee_user_id, deadline_id
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
    priority         = $6,
    due_date         = $7,
    assignee_user_id = $8
WHERE id = $1 AND tenant_id = $2
RETURNING id, tenant_id, court_record_id, deadline_id, intimation_id,
          title, description, kind, priority, due_date, status, source, assignee_user_id,
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

-- ── task_comment write path (discussion thread, §4/§10) ──────────────────────
-- The Tarefa detail's Comentários tab: a task grows N free-text comments. The write is keyed
-- by the parent task and scoped to tenant_id (barrier 1, on top of RLS barrier 2). The parent-
-- task guard (TaskExistsInTenant, reused) turns a foreign/unknown task_id into a typed 404
-- BEFORE the insert, so a comment can never be grafted onto another tenant's task.

-- name: InsertTaskComment :one
-- Append one comment to a task's thread (POST /v1/tasks/:id/comments). tenant_id/task_id come
-- from the request context + the guarded parent; author_user_id is the verified principal (never
-- the body). Returns the whole row so the handler renders it without a re-read. $1 = tenant_id,
-- $2 = task_id, $3 = author_user_id, $4 = body.
INSERT INTO task_comment (tenant_id, task_id, author_user_id, body)
VALUES ($1, $2, $3, $4)
RETURNING id, task_id, author_user_id, body, created_at;

-- ── task_activity write path (audit log, §4/§10) ─────────────────────────────
-- The Tarefa detail's Atividade tab: every meaningful mutation appends ONE row here, IN THE SAME
-- tx as the mutation. tenant_id/task_id are the mutated task; actor_user_id is the verified
-- principal; event_type is the closed set (entity.go). from_value/to_value carry a field change's
-- before/after (NULL for create/lifecycle/comment).

-- name: InsertTaskActivity :one
-- Append one audit-log row. $1 = tenant_id, $2 = task_id, $3 = actor_user_id, $4 = event_type,
-- $5 = from_value (nullable), $6 = to_value (nullable). Returns the id (the caller does not need
-- the row back — the Atividade tab re-reads the whole log).
INSERT INTO task_activity (tenant_id, task_id, actor_user_id, event_type, from_value, to_value)
VALUES ($1, $2, $3, $4, $5, $6)
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
SELECT id, court_record_id, notification_id, start_date, status, kind, days, counting, doubled,
       doubled_reason, anchor_event, manual_extra_days
FROM deadline
WHERE id = $1 AND tenant_id = $2;

-- name: UpdateDeadlineAdjust :one
-- Ajuste manual do prazo legal (§9: PATCH /v1/prazos/:id → recalcula datas). Writes the
-- patched {kind, days, counting, doubled, doubled_reason, anchor_event, manual_extra_days} and
-- the RECOMPUTED {end_date, holidays_applied, start_date}, keyed by id and scoped to tenant_id
-- (barrier 1). status is LEFT AS-IS: the ajuste never changes the lifecycle (a PENDING stays
-- PENDING, an OPEN stays OPEN — the use case already refused a terminal prazo); source/
-- rules_version/confirmed_* are untouched. start_date IS written now (the panel may re-anchor via
-- anchor_event → a different intimação date). A no-match (the
-- row vanished mid-tx) → pgx.ErrNoRows → ErrDeadlineNotFound at the mapper. Returns the prazo id
-- and the record it hangs on. $1 = id, $2 = tenant_id, then the patched fields.
UPDATE deadline
SET kind              = $3,
    days              = $4,
    counting          = $5,
    doubled           = $6,
    doubled_reason    = $7,
    end_date          = $8,
    holidays_applied  = $9,
    start_date        = sqlc.arg(start_date),
    anchor_event      = sqlc.arg(anchor_event),
    manual_extra_days = sqlc.arg(manual_extra_days)
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

-- name: GetDeadlineEndDate :one
-- Read ONLY a prazo's end_date by id, scoped to tenant_id (barrier 1, on top of RLS barrier 2).
-- The task write path (POST /v1/tasks, PATCH /v1/tasks/:id) reads it inside its tx to enforce
-- ERD §4's task invariant: a task's due_date cannot fall after its prazo's end_date. It is a
-- deliberately narrow read (one column) — the caller needs the end_date alone, not the whole
-- prazo. A missing id in the tenant → pgx.ErrNoRows → typed ErrDeadlineNotFound at the mapper,
-- never (zero, nil). $1 = id, $2 = tenant_id.
SELECT end_date
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

-- name: ListReconcilableDeadlines :many
-- List the prazos of a court_record that are candidates for reconciliation by an
-- andamento de resposta (docs: prazo histórico nascido MISSED apesar de já haver
-- petição/manifestação nos autos). Scoped to (court_record_id, tenant_id) (barrier 1,
-- on top of RLS barrier 2). ONLY status IN ('MISSED','OPEN') qualify — the reconcile
-- resurrects a prazo dado por perdido/aberto, never a PENDING (unconfirmed suggestion),
-- a MET (already done) nor a CANCELLED (revoked). Each row carries the id (to flip) and
-- the fixed start_date the response-movement predicate compares occurred_at against — a
-- movimento só cumpre o prazo se ocorreu em/depois do início da contagem. No rows → an
-- empty slice (a record with no reconcilable prazo), never an error. $1 = court_record_id,
-- $2 = tenant_id, both from the trusted event payload.
SELECT id, start_date
FROM deadline
WHERE court_record_id = $1
  AND tenant_id = $2
  AND status IN ('MISSED', 'OPEN');

-- name: HasResponseMovement :one
-- Does the court_record hold an andamento de RESPOSTA on/after the prazo's start_date?
-- The predicate the reconcile hangs on (docs): a movimento é resposta quando seu tpu_code
-- está no conjunto de códigos de peça de resposta (@tpu_codes), OU — sem código TPU — quando
-- o texto casa o regex de tipos de peça (petiç|manifest|contestaç|impugnaç|recurso|embargos|
-- defesa). docket_entry has NO tenant_id of its own, so it is scoped by JOIN court_record +
-- the explicit tenant filter (barrier 1) on top of RLS (barrier 2) — a movimento can never
-- leak across tenants. occurred_at >= start_date anchors it to the contagem: a peça anterior
-- ao início do prazo não o cumpre. Returns a bool (EXISTS), never (nil, nil). $1 =
-- court_record_id, $2 = tenant_id, $3 = start_date, $4 = tpu_codes (int[]).
SELECT EXISTS (
    SELECT 1
    FROM docket_entry de
    JOIN court_record cr ON cr.id = de.court_record_id
    WHERE cr.id = $1
      AND cr.tenant_id = $2
      AND de.occurred_at >= $3
      AND (
        de.tpu_code = ANY(@tpu_codes::int[])
        OR (
          de.tpu_code IS NULL
          AND de.text ~* 'petiç|manifest|contestaç|impugnaç|recurso|embargos|defesa'
        )
      )
) AS has_response;

-- name: MarkMet :one
-- Reconcile a prazo MISSED/OPEN → MET, keyed by id and scoped to tenant_id (barrier 1). The
-- `status IN ('MISSED','OPEN')` guard makes the flip SAFE and IDEMPOTENT (docs: só ressuscita
-- um prazo dado por perdido/aberto): a redelivery, an already-MET/CANCELLED prazo, or a
-- PENDING one updates NO row → pgx.ErrNoRows → typed ErrDeadlineNotFound at the mapper, the
-- use case's no-op (never a phantom deadline.met). On a hit it returns the id so deadline.met
-- commits in the SAME tx. It mirrors MarkMissed's guarded-UPDATE shape, widened to the two
-- reconcilable statuses. $1 = id, $2 = tenant_id, both from the trusted event payload.
UPDATE deadline
SET status = 'MET'
WHERE id = $1 AND tenant_id = $2 AND status IN ('MISSED', 'OPEN')
RETURNING id;

-- name: MarkNoDeadline :one
-- Declare "mera ciência" on a prazo (§3 "Máquina de estados": PENDING|OPEN → NO_DEADLINE via
-- "Remover prazo" / "Não há prazo"), keyed by id and scoped to tenant_id (barrier 1). Stamps
-- confirmed_by/at (the human who declared it). The `status IN ('PENDING','OPEN')` guard makes
-- the flip SAFE and IDEMPOTENT and distinguishes the two client errors at the use case: a
-- missing id yields no row anyway (404), while a TERMINAL prazo (MET/MISSED/CANCELLED) also
-- yields no row — the use case pre-reads the status to return 409 for the latter. On a hit it
-- returns the id so deadline.no_deadline commits in the SAME tx. $1 = id, $2 = tenant_id,
-- $3 = confirmed_by, $4 = confirmed_at.
UPDATE deadline
SET status       = 'NO_DEADLINE',
    confirmed_by = $3,
    confirmed_at = $4
WHERE id = $1 AND tenant_id = $2 AND status IN ('PENDING', 'OPEN')
RETURNING id;

-- name: ReopenNoDeadline :one
-- Revert a "mera ciência" declaration (§3: NO_DEADLINE → PENDING via "reabrir"), keyed by id and
-- scoped to tenant_id (barrier 1). Clears confirmed_by/at (the prazo is a suggestion again). The
-- `status = 'NO_DEADLINE'` guard makes it SAFE and IDEMPOTENT: a prazo in any other status
-- updates NO row → pgx.ErrNoRows → typed ErrDeadlineNotFound at the mapper; the use case
-- pre-reads the status to distinguish a 404 miss from a 409 not-NO_DEADLINE. On a hit it returns
-- the id so deadline.reopened commits in the SAME tx. $1 = id, $2 = tenant_id.
UPDATE deadline
SET status       = 'PENDING',
    confirmed_by = NULL,
    confirmed_at = NULL
WHERE id = $1 AND tenant_id = $2 AND status = 'NO_DEADLINE'
RETURNING id;

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

-- name: SetDeadlineAISummary :execrows
-- Write-through, best-effort persist of the "O que aconteceu"/"O que fazer" summary
-- (migration 0036), called from the read path (GET /v1/prazos/:id/suggested-tasks) right
-- after the LLM answers, ONLY the first time (the suggester checks
-- GetPrazoSuggestContext.ai_summary_generated_at first). `ai_summary IS NULL` makes this
-- write-once/idempotent at the DB layer too: a redelivered or racing call is a safe no-op
-- (0 rows affected, not an error) — no invalidation/regenerate path exists by design.
-- Scoped to tenant_id (barrier 1, on top of RLS barrier 2). $1 = summary, $2 =
-- recommendation, $3 = deadline id, $4 = tenant_id.
UPDATE deadline
SET ai_summary = $1,
    ai_recommendation = $2,
    ai_summary_generated_at = now()
WHERE id = $3 AND tenant_id = $4 AND ai_summary IS NULL;

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

-- name: DeleteTasksByDeadline :exec
-- Drop every task of a confirmed prazo, the REPLACE step of the F2 confirm (§9: the
-- confirm is an "upsert idempotente por intimation_id"). Confirm runs this in the SAME tx
-- right after ConfirmDeadline and BEFORE re-inserting the submitted tasks, so re-confirming
-- the same intimação leaves EXACTLY the last submit's set instead of accumulating +N rows
-- each call. Scoped to tenant_id (barrier 1, on top of RLS barrier 2). A first confirm (no
-- prior tasks) deletes nothing — a clean no-op, never an error. $1 = deadline_id, $2 =
-- tenant_id, both from the confirm tx (the id ConfirmDeadline returned + the principal).
DELETE FROM task
WHERE deadline_id = $1 AND tenant_id = $2;

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

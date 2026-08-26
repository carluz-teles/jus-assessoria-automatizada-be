-- name: ResolveCourtRecordIDForDraftIntimation :one
-- Resolves the court_record a draft belongs to, via its intimation (the only path a
-- draft carries back to a process today — draft has no court_record_id column of its
-- own; draft.case_id is nullable and, even when set, a court_case can have more than
-- one court_record (1º/2º grau), so it is not a reliable 1:1 resolution). Used by the
-- activity listener (review.completed → DRAFT_GENERATED) to know which process's
-- timeline to append to. Scoped by tenant_id on BOTH draft and intimation (barrier 1).
-- No rows when the draft has no intimation_id (a blank/processo draft) — the caller
-- treats that as LOG-NOT-FAIL (nothing to log against).
SELECT i.court_record_id
FROM draft d
JOIN intimation i ON i.id = d.intimation_id
WHERE d.id = @draft_id::uuid
  AND d.tenant_id = @tenant_id::uuid
  AND i.tenant_id = @tenant_id::uuid;

-- name: ListProcessActivityLog :many
-- The process cockpit's "Atividade" timeline (migration 0073): every logged event for
-- one court_record, newest first. Descending keyset on (occurred_at, id) — served by
-- the migration's index on (court_record_id, occurred_at DESC, id DESC) — the first
-- page passes the max sentinel ('9999-12-31T23:59:59Z', max-uuid). Scoped by tenant_id
-- (barrier 1) + RLS (barrier 2, migration 0073's tenant_isolation policy).
SELECT id, event_type, payload, occurred_at
FROM process_activity_log
WHERE court_record_id = @court_record_id::uuid
  AND tenant_id = @tenant_id::uuid
  AND (occurred_at, id) < (@last_occurred::timestamptz, @last_id::uuid)
ORDER BY occurred_at DESC, id DESC
LIMIT @page_limit;

-- name: CountProcessActivityLog :one
-- The "X de Y" total for the Atividade tab: how many activity rows the process holds.
-- Scoped by the same court_record_id + tenant_id as the list.
SELECT count(*) FROM process_activity_log
WHERE court_record_id = @court_record_id::uuid
  AND tenant_id = @tenant_id::uuid;

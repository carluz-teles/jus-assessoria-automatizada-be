-- triagem da intimação — the write path for POST /v1/intimacoes/:id/{resolve,
-- ignore,reopen}. The user drives the intimation's workflow state (user_status,
-- 0030), SEPARATE from the DJEN cancellation `status`. Runs inside the caller's tx
-- (UoW + SET LOCAL app.tenant_id), so RLS is a second barrier under the explicit
-- tenant_id filter. No outbox event yet — there is no consumer of a triagem fact —
-- so this is a bare projection write (a future slice can emit in this same tx).

-- name: SetIntimationUserStatus :one
-- Set the intimation's triagem state (user_status) to $3, tenant-scoped (barrier 1,
-- RLS barrier 2). The three actions map to the three target states: resolve→RESOLVED,
-- ignore→IGNORED, reopen→PENDING. Only user_status is touched — the DJEN `status`
-- (ACTIVE/CANCELLED) is left alone. A zero-row result (unknown id or a foreign tenant's
-- row) surfaces as pgx.ErrNoRows → ErrIntimationNotFound (→ 404), never nil,nil.
-- Idempotent: re-resolving an already-RESOLVED row rewrites the same value.
UPDATE intimation
   SET user_status = $3
 WHERE id = $1 AND tenant_id = $2
RETURNING id;

-- name: AssignIntimationResponsaveis :one
-- Set (or clear, via NULL) the conductor_user_id and reviewer_user_id on one intimation,
-- tenant-scoped (barrier 1, RLS barrier 2). Both are nullable: passing NULL desatribui
-- the role. The use case pre-validates membership (AppUserExistsInTenant) before calling
-- this, so the FK is always satisfied when non-null. A zero-row result (unknown id or
-- foreign tenant's row) surfaces as pgx.ErrNoRows → ErrIntimationNotFound (→ 404).
-- Idempotent: re-assigning the same user re-writes the same value.
UPDATE intimation
   SET conductor_user_id = sqlc.narg('conductor_user_id')::uuid,
       reviewer_user_id  = sqlc.narg('reviewer_user_id')::uuid
 WHERE id = $1 AND tenant_id = $2
RETURNING id;

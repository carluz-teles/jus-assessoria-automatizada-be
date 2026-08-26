-- responsável do processo (case-level) — the write path for PUT
-- /v1/processos/:id/responsavel. All three run inside the caller's tx (UoW +
-- SET LOCAL app.tenant_id), so RLS is a second barrier under the explicit
-- tenant_id filters here. No outbox event yet — auditoria/evento is a future
-- slice (there is no consumer), so this is a bare projection write.

-- name: GetCaseIDByCourtRecord :one
-- Resolve the court_case behind a court_record :id, tenant-scoped (barrier 1). The FE
-- addresses a process by its court_record id (the same id /processos returns), but the
-- responsável lives on court_case — so the write must hop record → case first. No row
-- (unknown id or a foreign tenant's record) → pgx.ErrNoRows, mapped to ErrProcessoNotFound
-- (→ 404) upstream, never nil,nil.
SELECT cr.case_id
FROM court_record cr
WHERE cr.id = $1 AND cr.tenant_id = $2;

-- name: AppUserExistsInTenant :one
-- Membership guard for the assign write: is $1 an app_user of tenant $2? Used only when
-- a non-null user_id is being assigned (desatribuir skips it). A false result → the
-- typed "usuário não é membro do escritório" (→ 404/invalid) so a caller cannot pin a
-- process on a user outside their escritório.
SELECT EXISTS (
    SELECT 1 FROM app_user au
    WHERE au.id = $1 AND au.tenant_id = $2
) AS present;

-- name: AssignCaseResponsible :exec
-- Set (or clear, when $2 is NULL) the responsável on a court_case, tenant-scoped
-- (barrier 1, with RLS as barrier 2). The case_id is the one resolved from the
-- court_record above; the WHERE ties the write to the caller's own tenant so a mismatched
-- (case, tenant) touches nothing. Idempotent — re-assigning the same user is a no-op write.
UPDATE court_case
   SET assigned_user_id = $2
 WHERE id = $1 AND tenant_id = $3;

-- name: CascadeCaseResponsibleToIntimations :execrows
-- Cascateia o responsável do court_case para as intimações filhas, na MESMA tx do
-- AssignResponsible. Filtra por court_record_id → court_record.case_id (não por
-- intimation.case_id direto): é essa a coluna que todo READ do app usa para decidir
-- "a qual processo esta intimação pertence". Um UPDATE ... FROM, O(1) statements
-- independente de N filhas. NULL desatribui (mesma semântica do pai).
UPDATE intimation i
   SET assignee_user_id = sqlc.narg('assignee_user_id')::uuid
  FROM court_record cr
 WHERE i.court_record_id = cr.id
   AND cr.case_id = @case_id::uuid
   AND i.tenant_id = @tenant_id::uuid;

-- name: GetCaseAssignedUser :one
-- Lê o assigned_user_id vigente do court_case, dentro da tx do caller, tenant-scoped.
-- Usado no merge de grade (gradeInTx) para re-sincronizar o responsável do case de
-- DESTINO nas intimações repontadas, ANTES de cascatear.
SELECT assigned_user_id FROM court_case WHERE id = @case_id::uuid AND tenant_id = @tenant_id::uuid;

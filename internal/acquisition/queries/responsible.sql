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

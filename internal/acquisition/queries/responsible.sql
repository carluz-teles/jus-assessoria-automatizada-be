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

-- ── bulk (POST /v1/processos/bulk/responsavel) ──────────────────────────────────
-- Twins batchados do fluxo acima (GetCaseIDByCourtRecord → AssignCaseResponsible →
-- CascadeCaseResponsibleToIntimations), pra atribuir o responsável a vários
-- processos numa tx só. A FE endereça processos pelo court_record :id (mesma
-- granularidade do PUT single-item); o responsável vive no court_case (case-level,
-- compartilhado entre graus) — então o write resolve ids/filtro para os court_case
-- por trás e cascateia pras intimações filhas.

-- name: ResolveCaseIDsByRecordIDs :many
-- Gêmeo batchado de GetCaseIDByCourtRecord, pro modo "por ids" do bulk: resolve os
-- pares (record_id, case_id) para uma lista explícita, tenant-scoped (barrier 1).
-- Ids desconhecidos ou de outro tenant simplesmente somem do resultado (sem erro) —
-- o "affected" do caller é len(rows), mesma semântica de BulkAssignIntimacoesByIDs.
SELECT cr.id, cr.case_id
FROM court_record cr
WHERE cr.tenant_id = @tenant_id::uuid
  AND cr.id = ANY(@ids::uuid[]);

-- name: ResolveCaseIDsByProcessosFilter :many
-- Gêmeo de ResolveCaseIDsByRecordIDs pro modo "all" do bulk: reusa EXATAMENTE a
-- cláusula de filtro do ListProcessos (search/court/degree/lifecycle/assignee), de
-- modo que "all" aplica a TODA a faixa filtrada, não só à página carregada.
SELECT cr.id, cr.case_id
FROM court_record cr
LEFT JOIN court_case cc ON cc.id = cr.case_id
WHERE cr.tenant_id = @tenant_id::uuid
  AND (@search::text = '' OR cr.cnj_number ILIKE '%' || @search || '%' ESCAPE '\')
  AND (@court::text = '' OR cr.court = @court::text)
  AND (@degree::text = '' OR cr.degree = @degree::text)
  AND (@lifecycle::text = '' OR cr.lifecycle = @lifecycle::text)
  AND (@lifecycle::text <> '' OR cr.lifecycle = 'ACTIVE')
  AND (sqlc.narg('assignee_id')::uuid IS NULL OR cc.assigned_user_id = sqlc.narg('assignee_id')::uuid);

-- name: BulkAssignCaseResponsible :execrows
-- Atribuição em massa do responsável para uma lista de court_case ids (já resolvida
-- pelas duas queries acima), tenant-scoped (barrier 1, RLS barrier 2). NULL desatribui.
UPDATE court_case
   SET assigned_user_id = sqlc.narg('assignee_user_id')::uuid
 WHERE tenant_id = @tenant_id::uuid
   AND id = ANY(@case_ids::uuid[]);

-- name: BulkCascadeCaseResponsibleToIntimations :execrows
-- Gêmeo batchado de CascadeCaseResponsibleToIntimations: cascateia o mesmo
-- responsável para as intimações filhas de TODOS os court_case em @case_ids, na
-- MESMA tx do bulk assign acima. NULL desatribui (mesma semântica do pai).
UPDATE intimation i
   SET assignee_user_id = sqlc.narg('assignee_user_id')::uuid
  FROM court_record cr
 WHERE i.court_record_id = cr.id
   AND i.tenant_id = @tenant_id::uuid
   AND cr.case_id = ANY(@case_ids::uuid[]);

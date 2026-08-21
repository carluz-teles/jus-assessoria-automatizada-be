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

-- name: BulkAssignConductorByIDs :execrows
-- Atribuição em massa do condutor (SÓ conductor_user_id — preserva o revisor) para
-- uma lista explícita de ids, tenant-scoped (barrier 1, RLS barrier 2). NULL desatribui.
UPDATE intimation
   SET conductor_user_id = sqlc.narg('conductor_user_id')::uuid
 WHERE tenant_id = @tenant_id::uuid
   AND id = ANY(@ids::uuid[]);

-- name: BulkAssignConductorByFilter :execrows
-- Atribuição em massa do condutor para TODA a faixa/filtro atual (modo "todos" da UI —
-- inclui as linhas ainda não paginadas). Reusa EXATAMENTE a cláusula de filtro do
-- ListIntimacoes (search/type/user_status/court/assignee/urgencia) via subquery. SÓ toca
-- conductor_user_id (preserva o revisor). tenant-scoped (barrier 1, RLS barrier 2).
UPDATE intimation
   SET conductor_user_id = sqlc.narg('conductor_user_id')::uuid
 WHERE tenant_id = @tenant_id::uuid
   AND id IN (
     SELECT i.id
     FROM intimation i
     JOIN court_record cr ON cr.id = i.court_record_id
     LEFT JOIN deadline d ON d.notification_id = i.id AND d.tenant_id = i.tenant_id
     WHERE i.tenant_id = @tenant_id::uuid
       AND (
         @search::text = ''
         OR cr.cnj_number ILIKE '%' || @search || '%' ESCAPE '\'
         OR cr.class ILIKE '%' || @search || '%'
         OR cr.judging_body ILIKE '%' || @search || '%'
       )
       AND (@type::text = '' OR i.type = @type::text)
       AND (@user_status::text = '' OR i.user_status = @user_status::text)
       AND (@court::text = '' OR cr.court = @court::text)
       AND (
         sqlc.narg('assignee_id')::uuid IS NULL
         OR i.conductor_user_id = sqlc.narg('assignee_id')::uuid
         OR i.reviewer_user_id = sqlc.narg('assignee_id')::uuid
       )
       AND (
         @urgencia::text = ''
         OR (@urgencia::text = 'atraso'             AND (d.end_date - CURRENT_DATE) < 0  AND d.status IN ('PENDING', 'OPEN'))
         OR (@urgencia::text = 'hoje'               AND (d.end_date - CURRENT_DATE) = 0  AND d.status IN ('PENDING', 'OPEN'))
         OR (@urgencia::text = 'proximos_dois_dias' AND (d.end_date - CURRENT_DATE) BETWEEN 1 AND 2 AND d.status IN ('PENDING', 'OPEN'))
         OR (@urgencia::text = 'semana'             AND (d.end_date - CURRENT_DATE) BETWEEN 3 AND 7 AND d.status IN ('PENDING', 'OPEN'))
         OR (@urgencia::text = 'mais_adiante'       AND (d.end_date - CURRENT_DATE) > 7   AND d.status IN ('PENDING', 'OPEN'))
         OR (@urgencia::text = 'nao_confirmado'     AND d.status = 'PENDING')
         OR (@urgencia::text = 'sem_providencia'    AND i.ai_analyzed_at IS NULL AND i.user_status NOT IN ('RESOLVED', 'IGNORED'))
       )
   );

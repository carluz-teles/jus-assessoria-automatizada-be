-- analise.sql — queries for the AI analysis of one intimation (POST /v1/intimacoes/:id/analise).
-- GetIntimacaoAnaliseContext reads the teor + the court context the prompt needs; the write
-- query OVERWRITES the three ai_* columns (re-executable — "Gerar novamente" re-runs it).

-- name: GetIntimacaoAnaliseContext :one
-- One intimation's teor plus its court record's identification and the derived prazo end_date
-- (LEFT JOIN deadline on notification_id — NULL when no prazo yet; the IA uses it as the ceiling
-- for suggested due_dates). Scoped by tenant_id + intimation id (barrier 1). A miss/foreign row →
-- pgx.ErrNoRows → ErrIntimationNotFound (the same 404 semantics as GetIntimacao). type is nullable.
SELECT i.content, i.type,
       cr.cnj_number, cr.court, cr.degree, cr.class, cr.subject,
       d.end_date AS deadline_end_date
FROM intimation i
JOIN court_record cr ON cr.id = i.court_record_id
LEFT JOIN deadline d ON d.notification_id = i.id AND d.tenant_id = i.tenant_id
                     AND d.status IN ('PENDING', 'OPEN')
WHERE i.id = $1 AND i.tenant_id = $2
ORDER BY d.end_date ASC
LIMIT 1;

-- name: ListActiveMembers :many
-- The tenant's ACTIVE firm members (internal app_user id + name) — the assignable responsáveis
-- the analyze_intimation prompt lists so the IA suggests a real assignee. Same join as identity's
-- ListOrgMembers (membership status ACTIVE), projected to just id+name; ordered by name for a
-- stable prompt. Scoped by tenant_id (barrier 1). Reused only for the AI analysis context.
SELECT u.id, u.name
FROM membership m
JOIN app_user u ON u.id = m.app_user_id AND u.tenant_id = m.tenant_id
WHERE m.tenant_id = $1 AND m.status = 'ACTIVE'
ORDER BY u.name, u.id;

-- name: GetIntimationProvidenciasForUpdate :one
-- Reads the ai_providencias jsonb + ai_analyzed_at of one intimation, locking the row
-- (FOR UPDATE) so an aprovar/descartar read-modify-write is serialized. NULL ai_analyzed_at
-- (never analysed) vs a set stamp lets the use case reject a status change on an un-analysed
-- intimation. Scoped by tenant_id (barrier 1); a miss/foreign row → pgx.ErrNoRows.
SELECT ai_providencias, ai_analyzed_at
FROM intimation
WHERE id = $1 AND tenant_id = $2
FOR UPDATE;

-- name: SetIntimationProvidencias :exec
-- Overwrites ONLY the ai_providencias jsonb (leaves ai_summary / ai_analyzed_at untouched) —
-- the write half of the aprovar/descartar status flip. Scoped by tenant_id (barrier 1).
UPDATE intimation
SET ai_providencias = $1
WHERE id = $2 AND tenant_id = $3;

-- name: SetIntimationAIAnalysis :exec
-- Persists (OVERWRITES) the AI analysis of one intimation. Unlike SetCourtRecordAIResume
-- there is NO write-once guard — the analysis is re-executable ("Gerar novamente"). Scoped
-- by tenant_id (barrier 1). Degraded mode passes ai_summary='' + ai_providencias='[]'.
UPDATE intimation
SET ai_summary      = $1,
    ai_providencias = $2,
    ai_analyzed_at  = now()
WHERE id = $3
  AND tenant_id = $4;

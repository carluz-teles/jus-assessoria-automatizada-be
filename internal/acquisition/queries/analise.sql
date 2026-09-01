-- analise.sql — queries for the AI analysis of one intimation (POST /v1/intimacoes/:id/analise).
-- GetIntimacaoAnaliseContext reads the teor + the court context the prompt needs; the write
-- query OVERWRITES the ai_summary/ai_analyzed_at columns (re-executable — "Gerar novamente"
-- re-runs it). The providências themselves are NO LONGER persisted here as jsonb — Analisar
-- publishes acquisition.intimation.analyzed instead, and the actionitem slice's listener
-- materializes real action_item rows from it (docs/erd-costura-providencia-tarefa-peca.md).

-- name: GetIntimacaoAnaliseContext :one
-- One intimation's teor plus its court record's identification and the derived prazo end_date
-- (LEFT JOIN deadline on notification_id — NULL when no prazo yet; the IA uses it as the ceiling
-- for suggested due_dates). court_record_id/deadline_id are carried through so the caller can
-- stamp them onto the acquisition.intimation.analyzed event without a second round-trip — the
-- actionitem slice's materialized action_item rows need both. Scoped by tenant_id + intimation
-- id (barrier 1). A miss/foreign row → pgx.ErrNoRows → ErrIntimationNotFound (the same 404
-- semantics as GetIntimacao). type is nullable.
SELECT i.content, i.type,
       cr.id AS court_record_id, cr.cnj_number, cr.court, cr.degree, cr.class, cr.subject,
       d.id AS deadline_id, d.end_date AS deadline_end_date
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

-- name: SetIntimationAIAnalysis :one
-- Persists (OVERWRITES) the AI analysis of one intimation's ai_summary/ai_act/ai_analyzed_at.
-- Unlike SetCourtRecordAIResume there is NO write-once guard — the analysis is re-executable
-- ("Gerar novamente"). ai_providencias (migration 0051) is NO LONGER written here — the
-- providência candidates travel on acquisition.intimation.analyzed instead (see
-- analise_store.go), materialized by the actionitem slice into real action_item rows. Scoped
-- by tenant_id (barrier 1). Degraded mode passes ai_summary='' and ai_act=NULL. RETURNING
-- court_record_id so the caller can log a process_activity_log row in the SAME tx, without a
-- second round-trip to look up the owning court record.
UPDATE intimation
SET ai_summary      = $1,
    ai_act          = $2,
    ai_analyzed_at  = now()
WHERE id = $3
  AND tenant_id = $4
RETURNING court_record_id;

-- name: InsertProcessActivityLog :exec
-- Appends one row to the process's activity log (docs — Cockpit "Atividade" timeline).
-- event_type is the closed set the migration's CHECK enforces; payload carries the
-- event-specific detail as jsonb. Scoped by tenant_id (barrier 1) + RLS (barrier 2). Called
-- LOG-NOT-FAIL by producers: a failed insert here must never roll back the write it
-- documents, so callers log-and-continue on error instead of propagating it.
INSERT INTO process_activity_log (tenant_id, court_record_id, event_type, payload)
VALUES ($1, $2, $3, $4);

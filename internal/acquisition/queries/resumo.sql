-- resumo.sql — read-model queries for the AI process summary (GET /v1/processos/:id/resume).
-- The context is assembled by the repository from four narrow queries (one base row +
-- three :many reads) so sqlc types every column instead of a json_agg that would come
-- back as an opaque interface{}. The write query persists the AI summary best-effort
-- (WHERE ai_resume IS NULL makes it idempotent — a race between two first-opens is a
-- safe no-op on the loser).

-- name: GetResumoContext :one
-- One process's identification row plus the cached AI resume (if any). Scoped by
-- tenant_id + court_record_id (barrier 1). A miss/foreign row → pgx.ErrNoRows →
-- ErrProcessoNotFound (the same 404 semantics as GetProcesso).
SELECT
    cr.id,
    cr.cnj_number,
    cr.court,
    cr.degree,
    cr.class,
    cr.subject,
    cr.claim_value,
    cr.lifecycle,
    cr.filed_at,
    cr.judging_body,
    cr.ai_resume,
    cr.ai_resume_generated_at
FROM court_record cr
WHERE cr.id = $1 AND cr.tenant_id = $2;

-- name: GetResumoMovements :many
-- The last 10 docket entries for the process, newest first. Scoped by tenant through
-- the court_record join (docket_entry carries no tenant_id of its own).
SELECT de.occurred_at, de.text
FROM docket_entry de
JOIN court_record cr ON cr.id = de.court_record_id
WHERE de.court_record_id = $1
  AND cr.tenant_id = $2
ORDER BY de.occurred_at DESC
LIMIT 10;

-- name: GetResumoActiveIntimations :many
-- The intimations with status = 'ACTIVE' (the DJEN cancellation lifecycle), each with
-- the days of its derived deadline (deadline.days, via the 1:1 notification_id link;
-- NULL when the intimação has no prazo yet). tenant-scoped.
SELECT i.type, i.content AS teor, d.days AS deadline_days
FROM intimation i
LEFT JOIN deadline d ON d.notification_id = i.id
WHERE i.court_record_id = $1
  AND i.tenant_id = $2
  AND i.status = 'ACTIVE'
ORDER BY i.made_available_at DESC;

-- name: GetResumoOpenDeadlines :many
-- The prazos that are not closed (OPEN or PENDING — a prazo born PENDING is a rule
-- suggestion awaiting F2 confirmation, still relevant to the summary). days_left is
-- computed as calendar days to end_date, mirroring the prazos screen read. tenant-scoped.
SELECT d.kind, d.end_date, (d.end_date - CURRENT_DATE)::int AS days_left, d.counting
FROM deadline d
WHERE d.court_record_id = $1
  AND d.tenant_id = $2
  AND d.status IN ('OPEN', 'PENDING')
ORDER BY d.end_date ASC;

-- name: SetCourtRecordAIResume :exec
-- Best-effort persist of the AI-generated resume. The WHERE ai_resume IS NULL makes
-- the write idempotent at the DB layer — a race between two first-opens updates 0
-- rows on the loser, which is NOT an error (write-once by design).
UPDATE court_record
SET ai_resume = $1,
    ai_resume_generated_at = now()
WHERE id = $2
  AND tenant_id = $3
  AND ai_resume IS NULL;
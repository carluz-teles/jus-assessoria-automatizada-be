-- name: GetCoverageSummary :one
SELECT
    COALESCE(SUM(CASE WHEN tc.resultado = 'coberta' THEN 1 ELSE 0 END), 0)::int AS coberta,
    COALESCE(SUM(CASE WHEN tc.resultado = 'divergente' THEN 1 ELSE 0 END), 0)::int AS divergente,
    COALESCE(SUM(CASE WHEN tc.resultado = 'ausente' THEN 1 ELSE 0 END), 0)::int AS ausente
FROM thesis t
LEFT JOIN thesis_coverage tc ON tc.thesis_id = t.id
WHERE t.tenant_id = $1 AND t.draft_id = $2;

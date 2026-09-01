-- name: ListCoverageByDraft :many
SELECT tc.id, tc.thesis_id, tc.resultado, tc.detalhe, tc.created_at
FROM thesis_coverage tc
JOIN thesis t ON t.id = tc.thesis_id
WHERE t.tenant_id = $1 AND t.draft_id = $2
ORDER BY tc.created_at ASC;

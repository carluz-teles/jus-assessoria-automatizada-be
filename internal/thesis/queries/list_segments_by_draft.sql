-- name: ListSegmentsByDraft :many
SELECT id, tenant_id, draft_id, thesis_id, profile_section_id, conteudo, created_at
FROM draft_segment
WHERE tenant_id = $1 AND draft_id = $2
ORDER BY created_at ASC;

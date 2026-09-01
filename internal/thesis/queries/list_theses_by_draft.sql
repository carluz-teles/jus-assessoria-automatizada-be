-- name: ListThesesByDraft :many
SELECT id, tenant_id, draft_id, piece_profile_key, notification_id, enunciado, forca, estado, created_at
FROM thesis
WHERE tenant_id = $1 AND draft_id = $2
ORDER BY created_at ASC;

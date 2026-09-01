-- name: GetThesisByID :one
SELECT id, tenant_id, draft_id, piece_profile_key, notification_id, enunciado, forca, estado, created_at
FROM thesis
WHERE id = $1 AND tenant_id = $2;

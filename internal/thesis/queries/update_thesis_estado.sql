-- name: UpdateThesisEstado :one
UPDATE thesis
SET estado = $1
WHERE id = $2 AND tenant_id = $3
RETURNING id, tenant_id, draft_id, piece_profile_key, notification_id, enunciado, forca, estado, created_at;

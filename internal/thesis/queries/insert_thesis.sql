-- name: InsertThesis :one
INSERT INTO thesis (id, tenant_id, draft_id, piece_profile_key, notification_id, enunciado, forca, estado, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING id, tenant_id, draft_id, piece_profile_key, notification_id, enunciado, forca, estado, created_at;

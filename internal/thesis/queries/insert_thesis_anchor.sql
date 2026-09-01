-- name: InsertThesisAnchor :one
INSERT INTO thesis_anchor (id, thesis_id, tipo, alvo_documento, alvo_fonte, motivo, status)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING id, thesis_id, tipo, alvo_documento, alvo_fonte, motivo, status;

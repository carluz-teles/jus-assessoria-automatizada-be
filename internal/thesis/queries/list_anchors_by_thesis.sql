-- name: ListAnchorsByThesis :many
SELECT id, thesis_id, tipo, alvo_documento, alvo_fonte, motivo, status
FROM thesis_anchor
WHERE thesis_id = $1
ORDER BY id ASC;

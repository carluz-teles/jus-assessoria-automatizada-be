-- name: GetSectionsByProfile :many
SELECT id, piece_profile_key, key, titulo, ordem, obrigatoria, origem, aceita_teses, fonte_legal
FROM profile_section
WHERE piece_profile_key = $1
ORDER BY ordem;

-- name: InsertSection :one
INSERT INTO profile_section (piece_profile_key, key, titulo, ordem, obrigatoria, origem, aceita_teses, fonte_legal)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING id, piece_profile_key, key, titulo, ordem, obrigatoria, origem, aceita_teses, fonte_legal;

-- name: UpdateSection :one
UPDATE profile_section
SET key = COALESCE(NULLIF(sqlc.arg(new_key)::text, ''), key),
    titulo = COALESCE(NULLIF(sqlc.arg(titulo)::text, ''), titulo),
    ordem = CASE WHEN sqlc.arg(ordem)::int = 0 THEN ordem ELSE sqlc.arg(ordem)::int END,
    obrigatoria = COALESCE(NULLIF(sqlc.arg(obrigatoria)::text, ''), obrigatoria),
    origem = COALESCE(NULLIF(sqlc.arg(origem)::text, ''), origem),
    aceita_teses = sqlc.arg(aceita_teses),
    fonte_legal = sqlc.narg(fonte_legal)
WHERE id = sqlc.arg(id)
RETURNING id, piece_profile_key, key, titulo, ordem, obrigatoria, origem, aceita_teses, fonte_legal;

-- name: DeleteSection :execrows
DELETE FROM profile_section
WHERE id = $1;

-- name: InsertVersion :one
INSERT INTO piece_profile_version (piece_profile_key, version, vigente_desde, snapshot)
VALUES ($1, $2, $3, $4)
RETURNING id, piece_profile_key, version, vigente_desde, snapshot;

-- name: GetVersionByKeyAndVersion :one
SELECT id, piece_profile_key, version, vigente_desde, snapshot
FROM piece_profile_version
WHERE piece_profile_key = $1 AND version = $2;

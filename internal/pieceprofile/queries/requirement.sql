-- name: GetRequirementsByProfile :many
SELECT id, piece_profile_key, campo, obrigatorio, fonte_legal
FROM profile_requirement
WHERE piece_profile_key = $1;

-- name: InsertRequirement :one
INSERT INTO profile_requirement (piece_profile_key, campo, obrigatorio, fonte_legal)
VALUES ($1, $2, $3, $4)
RETURNING id, piece_profile_key, campo, obrigatorio, fonte_legal;

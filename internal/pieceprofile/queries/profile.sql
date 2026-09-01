-- name: GetProfileByKey :one
SELECT key, nome, polo, matter_key, base_skeleton_key, format_profile_key,
       version_atual, fonte_legal, created_at, updated_at
FROM piece_profile
WHERE key = $1;

-- name: ListProfiles :many
SELECT key, nome, polo, matter_key, base_skeleton_key, format_profile_key,
       version_atual, fonte_legal, created_at, updated_at
FROM piece_profile
WHERE (@matter_key::text = '' OR matter_key = @matter_key::text)
ORDER BY nome;

-- name: InsertProfile :one
INSERT INTO piece_profile (key, nome, polo, matter_key, base_skeleton_key, format_profile_key, version_atual, fonte_legal)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING key, nome, polo, matter_key, base_skeleton_key, format_profile_key,
          version_atual, fonte_legal, created_at, updated_at;

-- name: UpdateProfile :one
UPDATE piece_profile
SET nome = $2, polo = $3, matter_key = $4, base_skeleton_key = $5,
    format_profile_key = $6, fonte_legal = $7, updated_at = now()
WHERE key = $1
RETURNING key, nome, polo, matter_key, base_skeleton_key, format_profile_key,
          version_atual, fonte_legal, created_at, updated_at;

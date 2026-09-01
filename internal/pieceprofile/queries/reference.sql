-- name: GetMatterByKey :one
SELECT key, nome FROM matter WHERE key = $1;

-- name: ListMatters :many
SELECT key, nome FROM matter ORDER BY nome;

-- name: GetBaseSkeletonByKey :one
SELECT key, slots FROM base_skeleton WHERE key = $1;

-- name: ListBaseSkeletons :many
SELECT key, slots FROM base_skeleton ORDER BY key;

-- name: GetFormatProfileByKey :one
SELECT key, fonte, tamanho_corpo, tamanho_citacao_longa, espacamento, alinhamento,
       margens, citacao_longa, export
FROM format_profile WHERE key = $1;

-- name: ListFormatProfiles :many
SELECT key, fonte, tamanho_corpo, tamanho_citacao_longa, espacamento, alinhamento,
       margens, citacao_longa, export
FROM format_profile ORDER BY key;

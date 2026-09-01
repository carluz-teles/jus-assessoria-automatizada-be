-- name: UpdateRule :exec
UPDATE compliance_rule
SET descricao = COALESCE(sqlc.narg(descricao), descricao),
    severidade = COALESCE(sqlc.narg(severidade), severidade),
    fonte_legal = COALESCE(sqlc.narg(fonte_legal), fonte_legal),
    verificacao = COALESCE(sqlc.narg(verificacao), verificacao)
WHERE key = sqlc.arg(key);

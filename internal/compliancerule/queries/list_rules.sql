-- name: ListRules :many
SELECT key, descricao, severidade, fonte_legal, verificacao
FROM compliance_rule
ORDER BY key;

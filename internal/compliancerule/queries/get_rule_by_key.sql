-- name: GetRuleByKey :one
SELECT key, descricao, severidade, fonte_legal, verificacao
FROM compliance_rule
WHERE key = $1;

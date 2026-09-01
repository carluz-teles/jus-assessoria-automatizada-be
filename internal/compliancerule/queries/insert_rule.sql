-- name: InsertRule :exec
INSERT INTO compliance_rule (key, descricao, severidade, fonte_legal, verificacao)
VALUES ($1, $2, $3, $4, $5);

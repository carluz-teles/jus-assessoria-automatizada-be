-- name: DeleteRule :execrows
DELETE FROM compliance_rule
WHERE key = $1;

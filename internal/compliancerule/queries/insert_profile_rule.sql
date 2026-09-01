-- name: InsertProfileRule :execrows
INSERT INTO profile_rule (id, piece_profile_key, compliance_rule_key, override_severidade)
SELECT $1, pp.key, $3, sqlc.narg(override_severidade)
FROM piece_profile pp
WHERE pp.key = $2;

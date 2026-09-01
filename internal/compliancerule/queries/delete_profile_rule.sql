-- name: DeleteProfileRule :execrows
DELETE FROM profile_rule pr
USING piece_profile pp
WHERE pr.piece_profile_key = pp.key
  AND pp.key = $1
  AND pr.compliance_rule_key = $2;

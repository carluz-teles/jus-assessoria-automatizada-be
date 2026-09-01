-- name: DeleteSectionRule :execrows
DELETE FROM section_rule sr
USING profile_section ps
WHERE sr.profile_section_id = ps.id
  AND ps.id = $1
  AND sr.compliance_rule_key = $2;

-- name: ListSectionRules :many
SELECT sr.id, sr.profile_section_id, sr.compliance_rule_key
FROM section_rule sr
JOIN profile_section ps ON ps.id = sr.profile_section_id
WHERE sr.profile_section_id = $1
ORDER BY sr.compliance_rule_key;

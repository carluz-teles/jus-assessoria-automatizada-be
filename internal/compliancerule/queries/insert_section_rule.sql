-- name: InsertSectionRule :execrows
INSERT INTO section_rule (id, profile_section_id, compliance_rule_key)
SELECT $1, ps.id, $3
FROM profile_section ps
WHERE ps.id = $2;

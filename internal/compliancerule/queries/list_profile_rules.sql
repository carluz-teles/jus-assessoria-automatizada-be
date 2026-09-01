-- name: ListProfileRules :many
SELECT pr.id, pr.piece_profile_key, pr.compliance_rule_key, pr.override_severidade
FROM profile_rule pr
JOIN piece_profile pp ON pp.key = pr.piece_profile_key
WHERE pr.piece_profile_key = $1
ORDER BY pr.compliance_rule_key;

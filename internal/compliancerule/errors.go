package compliancerule

import "github.com/jusassessoria/platform/lib/apperr"

var (
	ErrComplianceRuleNotFound = apperr.NewNotFound("compliance rule not found")
	ErrProfileRuleNotFound    = apperr.NewNotFound("profile rule not found")
	ErrSectionRuleNotFound    = apperr.NewNotFound("section rule not found")
	// ErrPieceProfileNotFound/ErrProfileSectionNotFound guard the INSERT ... SELECT ...
	// FROM piece_profile/profile_section pattern (insert_profile_rule.sql,
	// insert_section_rule.sql): a foreign/unknown key makes the SELECT match zero rows,
	// so the INSERT silently affects zero rows instead of erroring — :execrows lets the
	// repo turn that into a typed 404 rather than a false-success 201.
	ErrPieceProfileNotFound   = apperr.NewNotFound("piece profile not found")
	ErrProfileSectionNotFound = apperr.NewNotFound("profile section not found")
)

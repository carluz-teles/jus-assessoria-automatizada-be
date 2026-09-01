package compliancerule

import (
	"github.com/google/uuid"

	"github.com/jusassessoria/platform/internal/compliancerule/complianceruledb"
	"github.com/jusassessoria/platform/lib/database"
)

// mapper.go is the boundary where driver types die: the entity and the use case
// stay pure Go. The repo returns ComplianceRule/ProfileRule/SectionRule values,
// never the sqlc row.

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func textToNull(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// parseUUID parses an id that came from a path param. A malformed value is an
// infra fault, wrapped so the edge treats it as 500 and the cause is logged.
func parseUUID(s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.UUID{}, database.WrapInfra(err)
	}
	return id, nil
}

func ruleFromRow(r complianceruledb.ComplianceRule) ComplianceRule {
	return ComplianceRule{
		Key:         r.Key,
		Descricao:   r.Descricao,
		Severidade:  Severidade(r.Severidade),
		FonteLegal:  derefString(r.FonteLegal),
		Verificacao: Verificacao(r.Verificacao),
	}
}

func profileRuleFromRow(r complianceruledb.ProfileRule) ProfileRule {
	return ProfileRule{
		ID:                 r.ID.String(),
		PieceProfileKey:    r.PieceProfileKey,
		ComplianceRuleKey:  r.ComplianceRuleKey,
		OverrideSeveridade: Severidade(derefString(r.OverrideSeveridade)),
	}
}

func sectionRuleFromRow(r complianceruledb.SectionRule) SectionRule {
	return SectionRule{
		ID:                r.ID.String(),
		ProfileSectionID:  r.ProfileSectionID.String(),
		ComplianceRuleKey: r.ComplianceRuleKey,
	}
}

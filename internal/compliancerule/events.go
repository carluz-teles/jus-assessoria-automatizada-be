package compliancerule

import "github.com/jusassessoria/platform/lib/events"

// These event types are defined for API symmetry with the other slices in this
// feature (pieceprofile, thesis); nothing publishes them yet because compliance_rule
// mutations do not go through a UnitOfWork tx (see repository.go) — wiring an outbox
// publish here would require adding that transaction boundary, which is out of
// scope for this port. Left as a documented follow-up.
const (
	TypeComplianceRuleCreated = "compliancerule.rule_created"
	TypeComplianceRuleUpdated = "compliancerule.rule_updated"
	TypeComplianceRuleDeleted = "compliancerule.rule_deleted"
)

type ComplianceRuleCreated struct {
	events.Base
	RuleKey string `json:"rule_key"`
}

var _ events.Event = ComplianceRuleCreated{}

func (ComplianceRuleCreated) Type() string          { return TypeComplianceRuleCreated }
func (ComplianceRuleCreated) AggregateType() string { return "compliance_rule" }

type ComplianceRuleUpdated struct {
	events.Base
	RuleKey string `json:"rule_key"`
}

var _ events.Event = ComplianceRuleUpdated{}

func (ComplianceRuleUpdated) Type() string          { return TypeComplianceRuleUpdated }
func (ComplianceRuleUpdated) AggregateType() string { return "compliance_rule" }

type ComplianceRuleDeleted struct {
	events.Base
	RuleKey string `json:"rule_key"`
}

var _ events.Event = ComplianceRuleDeleted{}

func (ComplianceRuleDeleted) Type() string          { return TypeComplianceRuleDeleted }
func (ComplianceRuleDeleted) AggregateType() string { return "compliance_rule" }

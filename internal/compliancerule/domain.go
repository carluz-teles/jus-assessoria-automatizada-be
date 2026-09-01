package compliancerule

import (
	"context"
)

// Repository is the persistence port for compliance_rule (global catalog) and its
// profile_rule/section_rule links (also global — piece_profile has no tenant_id,
// docs/erd-tipos-de-peca.md §7.1). tenantID is threaded through the link methods
// for signature consistency with the rest of the platform (CLAUDE.md), even
// though it is not used to filter these tables.
type Repository interface {
	GetRuleByKey(ctx context.Context, key string) (ComplianceRule, error)
	ListRules(ctx context.Context) ([]ComplianceRule, error)
	InsertRule(ctx context.Context, rule ComplianceRule) (ComplianceRule, error)
	UpdateRule(ctx context.Context, key string, cmd UpdateRuleCommand) (ComplianceRule, error)
	DeleteRule(ctx context.Context, key string) error

	ListProfileRules(ctx context.Context, tenantID, profileKey string) ([]ProfileRule, error)
	InsertProfileRule(ctx context.Context, tenantID, profileKey, ruleKey string, override *string) (ProfileRule, error)
	DeleteProfileRule(ctx context.Context, tenantID, profileKey, ruleKey string) error

	ListSectionRules(ctx context.Context, tenantID, sectionID string) ([]SectionRule, error)
	InsertSectionRule(ctx context.Context, tenantID, sectionID, ruleKey string) (SectionRule, error)
	DeleteSectionRule(ctx context.Context, tenantID, sectionID, ruleKey string) error
}

type UseCase struct {
	repo Repository
}

func NewUseCase(repo Repository) *UseCase {
	return &UseCase{repo: repo}
}

func (uc *UseCase) GetRule(ctx context.Context, key string) (ComplianceRule, error) {
	return uc.repo.GetRuleByKey(ctx, key)
}

func (uc *UseCase) ListRules(ctx context.Context) ([]ComplianceRule, error) {
	return uc.repo.ListRules(ctx)
}

type CreateRuleCommand struct {
	Key         string
	Descricao   string
	Severidade  string
	FonteLegal  string
	Verificacao string
}

func (uc *UseCase) CreateRule(ctx context.Context, cmd CreateRuleCommand) (ComplianceRule, error) {
	rule := ComplianceRule{
		Key:         cmd.Key,
		Descricao:   cmd.Descricao,
		Severidade:  Severidade(cmd.Severidade),
		FonteLegal:  cmd.FonteLegal,
		Verificacao: Verificacao(cmd.Verificacao),
	}
	return uc.repo.InsertRule(ctx, rule)
}

type UpdateRuleCommand struct {
	Key         string
	Descricao   *string
	Severidade  *string
	FonteLegal  *string
	Verificacao *string
}

func (uc *UseCase) UpdateRule(ctx context.Context, cmd UpdateRuleCommand) (ComplianceRule, error) {
	return uc.repo.UpdateRule(ctx, cmd.Key, cmd)
}

func (uc *UseCase) DeleteRule(ctx context.Context, key string) error {
	return uc.repo.DeleteRule(ctx, key)
}

func (uc *UseCase) ListRulesByProfile(ctx context.Context, tenantID, profileKey string) ([]ProfileRule, error) {
	return uc.repo.ListProfileRules(ctx, tenantID, profileKey)
}

type AddProfileRuleCommand struct {
	TenantID           string
	PieceProfileKey    string
	RuleKey            string
	OverrideSeveridade *string
}

func (uc *UseCase) AddRuleToProfile(ctx context.Context, cmd AddProfileRuleCommand) (ProfileRule, error) {
	return uc.repo.InsertProfileRule(ctx, cmd.TenantID, cmd.PieceProfileKey, cmd.RuleKey, cmd.OverrideSeveridade)
}

func (uc *UseCase) RemoveRuleFromProfile(ctx context.Context, tenantID, profileKey, ruleKey string) error {
	return uc.repo.DeleteProfileRule(ctx, tenantID, profileKey, ruleKey)
}

func (uc *UseCase) ListRulesBySection(ctx context.Context, tenantID, sectionID string) ([]SectionRule, error) {
	return uc.repo.ListSectionRules(ctx, tenantID, sectionID)
}

type AddSectionRuleCommand struct {
	TenantID         string
	ProfileSectionID string
	RuleKey          string
}

func (uc *UseCase) AddRuleToSection(ctx context.Context, cmd AddSectionRuleCommand) (SectionRule, error) {
	return uc.repo.InsertSectionRule(ctx, cmd.TenantID, cmd.ProfileSectionID, cmd.RuleKey)
}

func (uc *UseCase) RemoveRuleFromSection(ctx context.Context, tenantID, sectionID, ruleKey string) error {
	return uc.repo.DeleteSectionRule(ctx, tenantID, sectionID, ruleKey)
}

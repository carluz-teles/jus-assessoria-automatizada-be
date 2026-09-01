package compliancerule

import (
	"context"
	"errors"
	"testing"
)

// mockRepo is a hand-rolled Repository: each method returns a configured value and
// records what it was asked.
type mockRepo struct {
	rule    ComplianceRule
	ruleErr error

	insertedRule ComplianceRule
	insertErr    error

	updateErr error

	deleteErr error

	profileRules    []ProfileRule
	profileRulesErr error

	insertedProfileRule ProfileRule
	insertProfileErr    error

	deleteProfileRuleErr error

	gotProfileKey string
	gotRuleKey    string
	gotOverride   *string
}

func (m *mockRepo) GetRuleByKey(ctx context.Context, key string) (ComplianceRule, error) {
	return m.rule, m.ruleErr
}

func (m *mockRepo) ListRules(ctx context.Context) ([]ComplianceRule, error) {
	return []ComplianceRule{m.rule}, nil
}

func (m *mockRepo) InsertRule(ctx context.Context, rule ComplianceRule) (ComplianceRule, error) {
	if m.insertErr != nil {
		return ComplianceRule{}, m.insertErr
	}
	return rule, nil
}

func (m *mockRepo) UpdateRule(ctx context.Context, key string, cmd UpdateRuleCommand) (ComplianceRule, error) {
	if m.updateErr != nil {
		return ComplianceRule{}, m.updateErr
	}
	return m.rule, nil
}

func (m *mockRepo) DeleteRule(ctx context.Context, key string) error {
	return m.deleteErr
}

func (m *mockRepo) ListProfileRules(ctx context.Context, tenantID, profileKey string) ([]ProfileRule, error) {
	return m.profileRules, m.profileRulesErr
}

func (m *mockRepo) InsertProfileRule(ctx context.Context, tenantID, profileKey, ruleKey string, override *string) (ProfileRule, error) {
	m.gotProfileKey = profileKey
	m.gotRuleKey = ruleKey
	m.gotOverride = override
	if m.insertProfileErr != nil {
		return ProfileRule{}, m.insertProfileErr
	}
	return m.insertedProfileRule, nil
}

func (m *mockRepo) DeleteProfileRule(ctx context.Context, tenantID, profileKey, ruleKey string) error {
	return m.deleteProfileRuleErr
}

func (m *mockRepo) ListSectionRules(ctx context.Context, tenantID, sectionID string) ([]SectionRule, error) {
	return nil, nil
}

func (m *mockRepo) InsertSectionRule(ctx context.Context, tenantID, sectionID, ruleKey string) (SectionRule, error) {
	return SectionRule{}, nil
}

func (m *mockRepo) DeleteSectionRule(ctx context.Context, tenantID, sectionID, ruleKey string) error {
	return nil
}

func TestUseCase_CreateRule(t *testing.T) {
	t.Parallel()

	repo := &mockRepo{}
	uc := NewUseCase(repo)

	rule, err := uc.CreateRule(context.Background(), CreateRuleCommand{
		Key: "pedido_certo", Descricao: "pedido certo", Severidade: string(SeveridadeBloqueante),
		Verificacao: string(VerificacaoDeterministica),
	})
	if err != nil {
		t.Fatalf("CreateRule() error = %v", err)
	}
	if rule.Key != "pedido_certo" || rule.Severidade != SeveridadeBloqueante {
		t.Errorf("CreateRule() = %+v, unexpected", rule)
	}
}

func TestUseCase_DeleteRule(t *testing.T) {
	t.Parallel()

	t.Run("not found propagates typed error", func(t *testing.T) {
		t.Parallel()
		repo := &mockRepo{deleteErr: ErrComplianceRuleNotFound}
		uc := NewUseCase(repo)

		err := uc.DeleteRule(context.Background(), "unknown")
		if !errors.Is(err, ErrComplianceRuleNotFound) {
			t.Errorf("DeleteRule() error = %v, want ErrComplianceRuleNotFound", err)
		}
	})
}

func TestUseCase_AddRuleToProfile(t *testing.T) {
	t.Parallel()

	t.Run("happy path threads override severidade", func(t *testing.T) {
		t.Parallel()
		override := string(SeveridadeAviso)
		repo := &mockRepo{insertedProfileRule: ProfileRule{ID: "pr-1", PieceProfileKey: "contestacao", ComplianceRuleKey: "eventualidade"}}
		uc := NewUseCase(repo)

		pr, err := uc.AddRuleToProfile(context.Background(), AddProfileRuleCommand{
			TenantID: "tenant-1", PieceProfileKey: "contestacao", RuleKey: "eventualidade", OverrideSeveridade: &override,
		})
		if err != nil {
			t.Fatalf("AddRuleToProfile() error = %v", err)
		}
		if pr.ID != "pr-1" {
			t.Errorf("AddRuleToProfile() = %+v, unexpected", pr)
		}
		if repo.gotProfileKey != "contestacao" || repo.gotRuleKey != "eventualidade" {
			t.Errorf("repo got profileKey=%q ruleKey=%q, want contestacao/eventualidade", repo.gotProfileKey, repo.gotRuleKey)
		}
		if repo.gotOverride == nil || *repo.gotOverride != string(SeveridadeAviso) {
			t.Errorf("repo got override = %v, want aviso", repo.gotOverride)
		}
	})

	t.Run("unknown profile propagates typed not-found", func(t *testing.T) {
		t.Parallel()
		repo := &mockRepo{insertProfileErr: ErrPieceProfileNotFound}
		uc := NewUseCase(repo)

		_, err := uc.AddRuleToProfile(context.Background(), AddProfileRuleCommand{
			TenantID: "tenant-1", PieceProfileKey: "inexistente", RuleKey: "pedido_certo",
		})
		if !errors.Is(err, ErrPieceProfileNotFound) {
			t.Errorf("AddRuleToProfile() error = %v, want ErrPieceProfileNotFound", err)
		}
	})
}

func TestUseCase_RemoveRuleFromProfile(t *testing.T) {
	t.Parallel()

	repo := &mockRepo{deleteProfileRuleErr: ErrProfileRuleNotFound}
	uc := NewUseCase(repo)

	err := uc.RemoveRuleFromProfile(context.Background(), "tenant-1", "contestacao", "unknown")
	if !errors.Is(err, ErrProfileRuleNotFound) {
		t.Errorf("RemoveRuleFromProfile() error = %v, want ErrProfileRuleNotFound", err)
	}
}

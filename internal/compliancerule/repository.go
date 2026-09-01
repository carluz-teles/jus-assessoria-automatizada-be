package compliancerule

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jusassessoria/platform/internal/compliancerule/complianceruledb"
	"github.com/jusassessoria/platform/lib/database"
)

// pgRepository is the sqlc-backed Repository, bound once at construction to the
// database handle it is given (the pool in production, a tx in tests) — unlike the
// other slices in this feature, compliance_rule/profile_rule/section_rule mutations
// have no multi-step invariant that needs a use-case-owned transaction boundary
// (each write is a single statement), so there is no UnitOfWork here.
type pgRepository struct {
	q *complianceruledb.Queries
}

var _ Repository = (*pgRepository)(nil)

// NewRepository returns the Repository bound to db (the shared pool). db satisfies
// the minimal Exec/Query/QueryRow subset sqlc's generated Queries needs.
func NewRepository(db database.Tx) Repository {
	return &pgRepository{q: complianceruledb.New(db)}
}

func (r *pgRepository) GetRuleByKey(ctx context.Context, key string) (ComplianceRule, error) {
	row, err := r.q.GetRuleByKey(ctx, key)
	if errors.Is(err, pgx.ErrNoRows) {
		return ComplianceRule{}, ErrComplianceRuleNotFound
	}
	if err != nil {
		return ComplianceRule{}, database.WrapInfra(err)
	}
	return ruleFromRow(row), nil
}

func (r *pgRepository) ListRules(ctx context.Context) ([]ComplianceRule, error) {
	rows, err := r.q.ListRules(ctx)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	out := make([]ComplianceRule, 0, len(rows))
	for _, row := range rows {
		out = append(out, ruleFromRow(row))
	}
	return out, nil
}

func (r *pgRepository) InsertRule(ctx context.Context, rule ComplianceRule) (ComplianceRule, error) {
	err := r.q.InsertRule(ctx, complianceruledb.InsertRuleParams{
		Key:         rule.Key,
		Descricao:   rule.Descricao,
		Severidade:  string(rule.Severidade),
		FonteLegal:  textToNull(rule.FonteLegal),
		Verificacao: string(rule.Verificacao),
	})
	if err != nil {
		return ComplianceRule{}, database.WrapInfra(err)
	}
	return rule, nil
}

func (r *pgRepository) UpdateRule(ctx context.Context, key string, cmd UpdateRuleCommand) (ComplianceRule, error) {
	if cmd.Descricao != nil || cmd.Severidade != nil || cmd.FonteLegal != nil || cmd.Verificacao != nil {
		if err := r.q.UpdateRule(ctx, complianceruledb.UpdateRuleParams{
			Descricao:   cmd.Descricao,
			Severidade:  cmd.Severidade,
			FonteLegal:  cmd.FonteLegal,
			Verificacao: cmd.Verificacao,
			Key:         key,
		}); err != nil {
			return ComplianceRule{}, database.WrapInfra(err)
		}
	}
	return r.GetRuleByKey(ctx, key)
}

func (r *pgRepository) DeleteRule(ctx context.Context, key string) error {
	rowsAffected, err := r.q.DeleteRule(ctx, key)
	if err != nil {
		return database.WrapInfra(err)
	}
	if rowsAffected == 0 {
		return ErrComplianceRuleNotFound
	}
	return nil
}

func (r *pgRepository) ListProfileRules(ctx context.Context, tenantID, profileKey string) ([]ProfileRule, error) {
	rows, err := r.q.ListProfileRules(ctx, profileKey)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	out := make([]ProfileRule, 0, len(rows))
	for _, row := range rows {
		out = append(out, profileRuleFromRow(row))
	}
	return out, nil
}

func (r *pgRepository) InsertProfileRule(ctx context.Context, tenantID, profileKey, ruleKey string, override *string) (ProfileRule, error) {
	id := uuid.New()
	var overrideVal *string
	if override != nil && *override != "" {
		overrideVal = override
	}

	rowsAffected, err := r.q.InsertProfileRule(ctx, complianceruledb.InsertProfileRuleParams{
		ID:                 id,
		Key:                profileKey,
		ComplianceRuleKey:  ruleKey,
		OverrideSeveridade: overrideVal,
	})
	if err != nil {
		return ProfileRule{}, database.WrapInfra(err)
	}
	if rowsAffected == 0 {
		return ProfileRule{}, ErrPieceProfileNotFound
	}
	return ProfileRule{
		ID:                 id.String(),
		PieceProfileKey:    profileKey,
		ComplianceRuleKey:  ruleKey,
		OverrideSeveridade: Severidade(derefString(overrideVal)),
	}, nil
}

func (r *pgRepository) DeleteProfileRule(ctx context.Context, tenantID, profileKey, ruleKey string) error {
	rowsAffected, err := r.q.DeleteProfileRule(ctx, complianceruledb.DeleteProfileRuleParams{
		Key:               profileKey,
		ComplianceRuleKey: ruleKey,
	})
	if err != nil {
		return database.WrapInfra(err)
	}
	if rowsAffected == 0 {
		return ErrProfileRuleNotFound
	}
	return nil
}

func (r *pgRepository) ListSectionRules(ctx context.Context, tenantID, sectionID string) ([]SectionRule, error) {
	id, err := parseUUID(sectionID)
	if err != nil {
		return nil, err
	}

	rows, err := r.q.ListSectionRules(ctx, id)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	out := make([]SectionRule, 0, len(rows))
	for _, row := range rows {
		out = append(out, sectionRuleFromRow(row))
	}
	return out, nil
}

func (r *pgRepository) InsertSectionRule(ctx context.Context, tenantID, sectionID, ruleKey string) (SectionRule, error) {
	sectionUUID, err := parseUUID(sectionID)
	if err != nil {
		return SectionRule{}, err
	}
	id := uuid.New()

	rowsAffected, err := r.q.InsertSectionRule(ctx, complianceruledb.InsertSectionRuleParams{
		ID:                id,
		ID_2:              sectionUUID,
		ComplianceRuleKey: ruleKey,
	})
	if err != nil {
		return SectionRule{}, database.WrapInfra(err)
	}
	if rowsAffected == 0 {
		return SectionRule{}, ErrProfileSectionNotFound
	}
	return SectionRule{
		ID:                id.String(),
		ProfileSectionID:  sectionID,
		ComplianceRuleKey: ruleKey,
	}, nil
}

func (r *pgRepository) DeleteSectionRule(ctx context.Context, tenantID, sectionID, ruleKey string) error {
	id, err := parseUUID(sectionID)
	if err != nil {
		return err
	}

	rowsAffected, err := r.q.DeleteSectionRule(ctx, complianceruledb.DeleteSectionRuleParams{
		ID:                id,
		ComplianceRuleKey: ruleKey,
	})
	if err != nil {
		return database.WrapInfra(err)
	}
	if rowsAffected == 0 {
		return ErrSectionRuleNotFound
	}
	return nil
}

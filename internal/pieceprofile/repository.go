package pieceprofile

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/jusassessoria/platform/internal/pieceprofile/pieceprofiledb"
	"github.com/jusassessoria/platform/lib/database"
)

// pgRepository is the sqlc-backed Repository. Every method binds the generated
// pieceprofiledb.Queries to the caller's tx; the repo holds no pool of its own — the
// use case owns the transaction boundary. piece_profile and its catalog children have
// no tenant_id (docs/erd-tipos-de-peca.md §7.1: v1 catalog is global), so tenantID is
// accepted for signature consistency but not used to filter these tables.
type pgRepository struct{}

var _ Repository = (*pgRepository)(nil)

// NewRepository returns the Repository. It is stateless: each method binds
// pieceprofiledb to the tx it is given, so there is nothing to inject at construction.
func NewRepository() Repository { return &pgRepository{} }

func (r *pgRepository) GetProfileByKey(ctx context.Context, tx database.Tx, tenantID, key string) (*PieceProfile, error) {
	row, err := pieceprofiledb.New(tx).GetProfileByKey(ctx, key)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrPieceProfileNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return profileFromRow(row), nil
}

func (r *pgRepository) ListProfiles(ctx context.Context, tx database.Tx, tenantID, matterKey string) ([]PieceProfile, error) {
	rows, err := pieceprofiledb.New(tx).ListProfiles(ctx, matterKey)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	out := make([]PieceProfile, 0, len(rows))
	for _, row := range rows {
		out = append(out, *profileFromRow(row))
	}
	return out, nil
}

func (r *pgRepository) InsertProfile(ctx context.Context, tx database.Tx, tenantID string, p *PieceProfile) (*PieceProfile, error) {
	row, err := pieceprofiledb.New(tx).InsertProfile(ctx, pieceprofiledb.InsertProfileParams{
		Key:              p.Key,
		Nome:             p.Nome,
		Polo:             p.Polo,
		MatterKey:        p.MatterKey,
		BaseSkeletonKey:  p.BaseSkeletonKey,
		FormatProfileKey: textToNull(p.FormatProfileKey),
		VersionAtual:     p.VersionAtual,
		FonteLegal:       p.FonteLegal,
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return profileFromRow(row), nil
}

func (r *pgRepository) UpdateProfile(ctx context.Context, tx database.Tx, tenantID, key string, p *PieceProfile) (*PieceProfile, error) {
	row, err := pieceprofiledb.New(tx).UpdateProfile(ctx, pieceprofiledb.UpdateProfileParams{
		Key:              key,
		Nome:             p.Nome,
		Polo:             p.Polo,
		MatterKey:        p.MatterKey,
		BaseSkeletonKey:  p.BaseSkeletonKey,
		FormatProfileKey: textToNull(p.FormatProfileKey),
		FonteLegal:       p.FonteLegal,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrPieceProfileNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return profileFromRow(row), nil
}

func (r *pgRepository) GetSectionsByProfile(ctx context.Context, tx database.Tx, tenantID, profileKey string) ([]ProfileSection, error) {
	rows, err := pieceprofiledb.New(tx).GetSectionsByProfile(ctx, profileKey)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	out := make([]ProfileSection, 0, len(rows))
	for _, row := range rows {
		out = append(out, *sectionFromRow(row))
	}
	return out, nil
}

func (r *pgRepository) InsertSection(ctx context.Context, tx database.Tx, tenantID string, s *ProfileSection) (*ProfileSection, error) {
	row, err := pieceprofiledb.New(tx).InsertSection(ctx, pieceprofiledb.InsertSectionParams{
		PieceProfileKey: s.PieceProfileKey,
		Key:             s.Key,
		Titulo:          s.Titulo,
		Ordem:           int32(s.Ordem),
		Obrigatoria:     s.Obrigatoria,
		Origem:          s.Origem,
		AceitaTeses:     s.AceitaTeses,
		FonteLegal:      bytesToText(s.FonteLegal),
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return sectionFromRow(row), nil
}

func (r *pgRepository) UpdateSection(ctx context.Context, tx database.Tx, tenantID, sectionID string, s *ProfileSection) (*ProfileSection, error) {
	id, err := parseUUID(sectionID)
	if err != nil {
		return nil, err
	}

	row, err := pieceprofiledb.New(tx).UpdateSection(ctx, pieceprofiledb.UpdateSectionParams{
		NewKey:      s.Key,
		Titulo:      s.Titulo,
		Ordem:       int32(s.Ordem),
		Obrigatoria: s.Obrigatoria,
		Origem:      s.Origem,
		AceitaTeses: s.AceitaTeses,
		FonteLegal:  bytesToText(s.FonteLegal),
		ID:          id,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrProfileSectionNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return sectionFromRow(row), nil
}

func (r *pgRepository) DeleteSection(ctx context.Context, tx database.Tx, tenantID, sectionID string) error {
	id, err := parseUUID(sectionID)
	if err != nil {
		return err
	}

	rowsAffected, err := pieceprofiledb.New(tx).DeleteSection(ctx, id)
	if err != nil {
		return database.WrapInfra(err)
	}
	if rowsAffected == 0 {
		return ErrProfileSectionNotFound
	}
	return nil
}

func (r *pgRepository) GetRequirementsByProfile(ctx context.Context, tx database.Tx, tenantID, profileKey string) ([]ProfileRequirement, error) {
	rows, err := pieceprofiledb.New(tx).GetRequirementsByProfile(ctx, profileKey)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	out := make([]ProfileRequirement, 0, len(rows))
	for _, row := range rows {
		out = append(out, *requirementFromRow(row))
	}
	return out, nil
}

func (r *pgRepository) InsertRequirement(ctx context.Context, tx database.Tx, tenantID string, req *ProfileRequirement) (*ProfileRequirement, error) {
	row, err := pieceprofiledb.New(tx).InsertRequirement(ctx, pieceprofiledb.InsertRequirementParams{
		PieceProfileKey: req.PieceProfileKey,
		Campo:           req.Campo,
		Obrigatorio:     req.Obrigatorio,
		FonteLegal:      bytesToText(req.FonteLegal),
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return requirementFromRow(row), nil
}

func (r *pgRepository) InsertVersion(ctx context.Context, tx database.Tx, tenantID string, v *PieceProfileVersion) (*PieceProfileVersion, error) {
	row, err := pieceprofiledb.New(tx).InsertVersion(ctx, pieceprofiledb.InsertVersionParams{
		PieceProfileKey: v.PieceProfileKey,
		Version:         v.Version,
		VigenteDesde:    pgTimestamptz(v.VigenteDesde),
		Snapshot:        v.Snapshot,
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return versionFromRow(row), nil
}

func (r *pgRepository) GetVersionByKeyAndVersion(ctx context.Context, tx database.Tx, tenantID, profileKey, version string) (*PieceProfileVersion, error) {
	row, err := pieceprofiledb.New(tx).GetVersionByKeyAndVersion(ctx, pieceprofiledb.GetVersionByKeyAndVersionParams{
		PieceProfileKey: profileKey,
		Version:         version,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrPieceProfileVersionNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return versionFromRow(row), nil
}

func (r *pgRepository) GetMatterByKey(ctx context.Context, tx database.Tx, key string) (*Matter, error) {
	row, err := pieceprofiledb.New(tx).GetMatterByKey(ctx, key)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrMatterNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return matterFromRow(row), nil
}

func (r *pgRepository) ListMatters(ctx context.Context, tx database.Tx) ([]Matter, error) {
	rows, err := pieceprofiledb.New(tx).ListMatters(ctx)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	out := make([]Matter, 0, len(rows))
	for _, row := range rows {
		out = append(out, *matterFromRow(row))
	}
	return out, nil
}

func (r *pgRepository) GetBaseSkeletonByKey(ctx context.Context, tx database.Tx, key string) (*BaseSkeleton, error) {
	row, err := pieceprofiledb.New(tx).GetBaseSkeletonByKey(ctx, key)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrBaseSkeletonNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return baseSkeletonFromRow(row), nil
}

func (r *pgRepository) ListBaseSkeletons(ctx context.Context, tx database.Tx) ([]BaseSkeleton, error) {
	rows, err := pieceprofiledb.New(tx).ListBaseSkeletons(ctx)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	out := make([]BaseSkeleton, 0, len(rows))
	for _, row := range rows {
		out = append(out, *baseSkeletonFromRow(row))
	}
	return out, nil
}

func (r *pgRepository) GetFormatProfileByKey(ctx context.Context, tx database.Tx, key string) (*FormatProfile, error) {
	row, err := pieceprofiledb.New(tx).GetFormatProfileByKey(ctx, key)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrFormatProfileNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return formatProfileFromRow(row), nil
}

func (r *pgRepository) ListFormatProfiles(ctx context.Context, tx database.Tx) ([]FormatProfile, error) {
	rows, err := pieceprofiledb.New(tx).ListFormatProfiles(ctx)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	out := make([]FormatProfile, 0, len(rows))
	for _, row := range rows {
		out = append(out, *formatProfileFromRow(row))
	}
	return out, nil
}

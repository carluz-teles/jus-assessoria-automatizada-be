package thesis

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/jusassessoria/platform/internal/thesis/thesisdb"
	"github.com/jusassessoria/platform/lib/database"
)

// Repository is the persistence port for the thesis contract model (docs/erd-
// costura-providencia-tarefa-peca.md §4): thesis/thesis_anchor are tenant-scoped via
// draft_id; draft_segment/segment_anchor/thesis_coverage follow the same scope.
type Repository interface {
	InsertThesis(ctx context.Context, tx database.Tx, t *Thesis) (*Thesis, error)
	GetThesisByID(ctx context.Context, tx database.Tx, tenantID, thesisID string) (*Thesis, error)
	ListThesesByDraft(ctx context.Context, tx database.Tx, tenantID, draftID string) ([]Thesis, error)
	UpdateThesisEstado(ctx context.Context, tx database.Tx, tenantID, thesisID, estado string) (*Thesis, error)

	InsertThesisAnchor(ctx context.Context, tx database.Tx, a *ThesisAnchor) (*ThesisAnchor, error)
	ListAnchorsByThesis(ctx context.Context, tx database.Tx, thesisID string) ([]ThesisAnchor, error)

	InsertDraftSegment(ctx context.Context, tx database.Tx, s *DraftSegment) (*DraftSegment, error)
	ListSegmentsByDraft(ctx context.Context, tx database.Tx, tenantID, draftID string) ([]DraftSegment, error)

	InsertSegmentAnchor(ctx context.Context, tx database.Tx, sa *SegmentAnchor) (*SegmentAnchor, error)
	ListAnchorsBySegment(ctx context.Context, tx database.Tx, segmentID string) ([]SegmentAnchor, error)

	InsertThesisCoverage(ctx context.Context, tx database.Tx, c *ThesisCoverage) (*ThesisCoverage, error)
	ListCoverageByDraft(ctx context.Context, tx database.Tx, tenantID, draftID string) ([]ThesisCoverage, error)
	GetCoverageSummary(ctx context.Context, tx database.Tx, tenantID, draftID string) (*CoverageSummary, error)
}

// pgRepository is the sqlc-backed Repository. Every method binds the generated
// thesisdb.Queries to the caller's tx (all reads and writes are transactional, so
// RLS scopes them to the event's tenant on thesis/draft_segment); the repo holds no
// pool of its own — the use case owns the boundary.
type pgRepository struct{}

var _ Repository = (*pgRepository)(nil)

// NewRepository returns the Repository. It is stateless: each method binds thesisdb
// to the tx it is given, so there is nothing to inject at construction.
func NewRepository() Repository { return &pgRepository{} }

func (r *pgRepository) InsertThesis(ctx context.Context, tx database.Tx, t *Thesis) (*Thesis, error) {
	id, err := parseUUID(t.ID)
	if err != nil {
		return nil, err
	}
	tenant, err := parseUUID(t.TenantID)
	if err != nil {
		return nil, err
	}
	draftID, err := parseUUID(t.DraftID)
	if err != nil {
		return nil, err
	}

	row, err := thesisdb.New(tx).InsertThesis(ctx, thesisdb.InsertThesisParams{
		ID:              id,
		TenantID:        tenant,
		DraftID:         draftID,
		PieceProfileKey: textToNull(t.PieceProfileKey),
		NotificationID:  pgOptionalUUID(t.NotificationID),
		Enunciado:       t.Enunciado,
		Forca:           t.Forca,
		Estado:          t.Estado,
		CreatedAt:       pgTimestamptz(t.CreatedAt),
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return thesisFromRow(row), nil
}

func (r *pgRepository) GetThesisByID(ctx context.Context, tx database.Tx, tenantID, thesisID string) (*Thesis, error) {
	id, err := parseUUID(thesisID)
	if err != nil {
		return nil, err
	}
	tenant, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}

	row, err := thesisdb.New(tx).GetThesisByID(ctx, thesisdb.GetThesisByIDParams{ID: id, TenantID: tenant})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrThesisNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return thesisFromRow(row), nil
}

func (r *pgRepository) ListThesesByDraft(ctx context.Context, tx database.Tx, tenantID, draftID string) ([]Thesis, error) {
	tenant, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}
	draft, err := parseUUID(draftID)
	if err != nil {
		return nil, err
	}

	rows, err := thesisdb.New(tx).ListThesesByDraft(ctx, thesisdb.ListThesesByDraftParams{TenantID: tenant, DraftID: draft})
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	out := make([]Thesis, 0, len(rows))
	for _, row := range rows {
		out = append(out, *thesisFromRow(row))
	}
	return out, nil
}

func (r *pgRepository) UpdateThesisEstado(ctx context.Context, tx database.Tx, tenantID, thesisID, estado string) (*Thesis, error) {
	id, err := parseUUID(thesisID)
	if err != nil {
		return nil, err
	}
	tenant, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}

	row, err := thesisdb.New(tx).UpdateThesisEstado(ctx, thesisdb.UpdateThesisEstadoParams{
		Estado:   estado,
		ID:       id,
		TenantID: tenant,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrThesisNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return thesisFromRow(row), nil
}

func (r *pgRepository) InsertThesisAnchor(ctx context.Context, tx database.Tx, a *ThesisAnchor) (*ThesisAnchor, error) {
	id, err := parseUUID(a.ID)
	if err != nil {
		return nil, err
	}
	thesisID, err := parseUUID(a.ThesisID)
	if err != nil {
		return nil, err
	}

	row, err := thesisdb.New(tx).InsertThesisAnchor(ctx, thesisdb.InsertThesisAnchorParams{
		ID:            id,
		ThesisID:      thesisID,
		Tipo:          a.Tipo,
		AlvoDocumento: pgOptionalUUID(a.AlvoDocumento),
		AlvoFonte:     textToNull(a.AlvoFonte),
		Motivo:        a.Motivo,
		Status:        a.Status,
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return thesisAnchorFromRow(row), nil
}

func (r *pgRepository) ListAnchorsByThesis(ctx context.Context, tx database.Tx, thesisID string) ([]ThesisAnchor, error) {
	id, err := parseUUID(thesisID)
	if err != nil {
		return nil, err
	}

	rows, err := thesisdb.New(tx).ListAnchorsByThesis(ctx, id)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	out := make([]ThesisAnchor, 0, len(rows))
	for _, row := range rows {
		out = append(out, *thesisAnchorFromRow(row))
	}
	return out, nil
}

func (r *pgRepository) InsertDraftSegment(ctx context.Context, tx database.Tx, s *DraftSegment) (*DraftSegment, error) {
	id, err := parseUUID(s.ID)
	if err != nil {
		return nil, err
	}
	tenant, err := parseUUID(s.TenantID)
	if err != nil {
		return nil, err
	}
	draftID, err := parseUUID(s.DraftID)
	if err != nil {
		return nil, err
	}
	thesisID, err := parseUUID(s.ThesisID)
	if err != nil {
		return nil, err
	}

	row, err := thesisdb.New(tx).InsertDraftSegment(ctx, thesisdb.InsertDraftSegmentParams{
		ID:               id,
		TenantID:         tenant,
		DraftID:          draftID,
		ThesisID:         thesisID,
		ProfileSectionID: pgOptionalUUID(s.ProfileSectionID),
		Conteudo:         s.Conteudo,
		CreatedAt:        pgTimestamptz(s.CreatedAt),
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return draftSegmentFromRow(row), nil
}

func (r *pgRepository) ListSegmentsByDraft(ctx context.Context, tx database.Tx, tenantID, draftID string) ([]DraftSegment, error) {
	tenant, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}
	draft, err := parseUUID(draftID)
	if err != nil {
		return nil, err
	}

	rows, err := thesisdb.New(tx).ListSegmentsByDraft(ctx, thesisdb.ListSegmentsByDraftParams{TenantID: tenant, DraftID: draft})
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	out := make([]DraftSegment, 0, len(rows))
	for _, row := range rows {
		out = append(out, *draftSegmentFromRow(row))
	}
	return out, nil
}

func (r *pgRepository) InsertSegmentAnchor(ctx context.Context, tx database.Tx, sa *SegmentAnchor) (*SegmentAnchor, error) {
	id, err := parseUUID(sa.ID)
	if err != nil {
		return nil, err
	}
	segmentID, err := parseUUID(sa.DraftSegmentID)
	if err != nil {
		return nil, err
	}
	anchorID, err := parseUUID(sa.ThesisAnchorID)
	if err != nil {
		return nil, err
	}

	row, err := thesisdb.New(tx).InsertSegmentAnchor(ctx, thesisdb.InsertSegmentAnchorParams{
		ID:             id,
		DraftSegmentID: segmentID,
		ThesisAnchorID: anchorID,
		Status:         sa.Status,
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return segmentAnchorFromRow(row), nil
}

func (r *pgRepository) ListAnchorsBySegment(ctx context.Context, tx database.Tx, segmentID string) ([]SegmentAnchor, error) {
	id, err := parseUUID(segmentID)
	if err != nil {
		return nil, err
	}

	rows, err := thesisdb.New(tx).ListAnchorsBySegment(ctx, id)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	out := make([]SegmentAnchor, 0, len(rows))
	for _, row := range rows {
		out = append(out, *segmentAnchorFromRow(row))
	}
	return out, nil
}

func (r *pgRepository) InsertThesisCoverage(ctx context.Context, tx database.Tx, c *ThesisCoverage) (*ThesisCoverage, error) {
	id, err := parseUUID(c.ID)
	if err != nil {
		return nil, err
	}
	thesisID, err := parseUUID(c.ThesisID)
	if err != nil {
		return nil, err
	}

	row, err := thesisdb.New(tx).InsertThesisCoverage(ctx, thesisdb.InsertThesisCoverageParams{
		ID:        id,
		ThesisID:  thesisID,
		Resultado: c.Resultado,
		Detalhe:   textToNull(c.Detalhe),
		CreatedAt: pgTimestamptz(c.CreatedAt),
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return thesisCoverageFromRow(row), nil
}

func (r *pgRepository) ListCoverageByDraft(ctx context.Context, tx database.Tx, tenantID, draftID string) ([]ThesisCoverage, error) {
	tenant, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}
	draft, err := parseUUID(draftID)
	if err != nil {
		return nil, err
	}

	rows, err := thesisdb.New(tx).ListCoverageByDraft(ctx, thesisdb.ListCoverageByDraftParams{TenantID: tenant, DraftID: draft})
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	out := make([]ThesisCoverage, 0, len(rows))
	for _, row := range rows {
		out = append(out, *thesisCoverageFromRow(row))
	}
	return out, nil
}

func (r *pgRepository) GetCoverageSummary(ctx context.Context, tx database.Tx, tenantID, draftID string) (*CoverageSummary, error) {
	tenant, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}
	draft, err := parseUUID(draftID)
	if err != nil {
		return nil, err
	}

	row, err := thesisdb.New(tx).GetCoverageSummary(ctx, thesisdb.GetCoverageSummaryParams{TenantID: tenant, DraftID: draft})
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return &CoverageSummary{
		Coberta:    int(row.Coberta),
		Divergente: int(row.Divergente),
		Ausente:    int(row.Ausente),
	}, nil
}

package draft

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/jusassessoria/platform/internal/draft/draftdb"
	"github.com/jusassessoria/platform/lib/database"
)

// Repository is the persistence port the use case depends on (never the concrete
// impl). Every method takes the caller's tx so it participates in the use case's
// unit of work; RLS scopes the reads/writes to the principal's tenant.
type Repository interface {
	// InsertDraft persists a new peça. On success it returns the full *Draft so the
	// handler renders the 201 response without a follow-up read. When the partial
	// unique index draft_intimation_id_uidx fires (ON CONFLICT DO NOTHING), it
	// returns ErrDraftAlreadyExists so the use case can fetch the existing row for 200.
	InsertDraft(ctx context.Context, tx database.Tx, d *Draft) (*Draft, error)
	// GetDraftByIntimationID returns the existing draft for the (tenant, intimation)
	// pair — the idempotent path after a 23505. A miss is ErrDraftNotFound.
	GetDraftByIntimationID(ctx context.Context, tx database.Tx, tenantID, intimationID string) (*Draft, error)
	// GetDraftByID returns the full draft by id, scoped to tenantID. A miss or
	// foreign-tenant id is ErrDraftNotFound (→ 404).
	GetDraftByID(ctx context.Context, tx database.Tx, tenantID, draftID string) (*Draft, error)
	// GetIntimationForDraft loads the minimal intimation context (case_id,
	// court_record_id, type) needed by the POST path when source=intimation. A miss
	// or foreign id is ErrIntimationNotFound (→ 404).
	GetIntimationForDraft(ctx context.Context, tx database.Tx, tenantID, intimationID string) (*IntimationContext, error)
	// UpdateDraftContent patches content (+optional title) and bumps updated_at. A
	// no-match (wrong id or foreign tenant) is ErrDraftNotFound.
	UpdateDraftContent(ctx context.Context, tx database.Tx, draftID, tenantID, content string, title *string) (*PatchResult, error)
	// GetDraftDetail runs the JOIN read model for GET /v1/pecas/:id. A miss is
	// ErrDraftNotFound.
	GetDraftDetail(ctx context.Context, tx database.Tx, tenantID, draftID string) (*DraftDetailView, error)
}

// ErrDraftAlreadyExists is a sentinel the repository returns when InsertDraft hits
// the partial unique index (23505). The use case treats this as idempotence and
// fetches the existing row for a 200 response — it is NOT an AppError because it
// never escapes to the client.
var ErrDraftAlreadyExists = errors.New("draft already exists for this intimation")

// pgRepository is the sqlc-backed Repository. Every method binds draftdb to the
// caller's tx; the repo is stateless (nothing to inject at construction).
type pgRepository struct{}

var _ Repository = (*pgRepository)(nil)

// NewRepository returns the stateless Repository.
func NewRepository() Repository { return &pgRepository{} }

func (r *pgRepository) InsertDraft(ctx context.Context, tx database.Tx, d *Draft) (*Draft, error) {
	tenantID, err := parseUUID(d.TenantID)
	if err != nil {
		return nil, err
	}

	// ON CONFLICT DO NOTHING: when the partial unique index fires (same tenant +
	// intimation_id), the INSERT is silently skipped and RETURNING yields no rows
	// (pgx.ErrNoRows). We map that to ErrDraftAlreadyExists so the use case can
	// fetch the existing row — the transaction stays healthy (no 23505 abort).
	row, err := draftdb.New(tx).InsertDraft(ctx, draftdb.InsertDraftParams{
		TenantID:     tenantID,
		CaseID:       optUUID(d.CaseID),
		IntimationID: optUUID(d.IntimationID),
		PieceType:    d.PieceType,
		Title:        d.Title,
		Content:      textToNull(d.Content),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrDraftAlreadyExists
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return draftFromInsertRow(row), nil
}

func (r *pgRepository) GetDraftByIntimationID(ctx context.Context, tx database.Tx, tenantID, intimationID string) (*Draft, error) {
	tid, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}
	iid := optUUID(intimationID)

	row, err := draftdb.New(tx).GetDraftByIntimationID(ctx, draftdb.GetDraftByIntimationIDParams{
		TenantID:     tid,
		IntimationID: iid,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrDraftNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return draftFromGetByIntimationRow(row), nil
}

func (r *pgRepository) GetDraftByID(ctx context.Context, tx database.Tx, tenantID, draftID string) (*Draft, error) {
	tid, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}
	did, err := parseUUID(draftID)
	if err != nil {
		return nil, err
	}

	row, err := draftdb.New(tx).GetDraftByID(ctx, draftdb.GetDraftByIDParams{
		ID:       did,
		TenantID: tid,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrDraftNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return draftFromGetByIDRow(row), nil
}

func (r *pgRepository) GetIntimationForDraft(ctx context.Context, tx database.Tx, tenantID, intimationID string) (*IntimationContext, error) {
	tid, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}
	iid, err := parseUUID(intimationID)
	if err != nil {
		return nil, err
	}

	row, err := draftdb.New(tx).GetIntimationForDraft(ctx, draftdb.GetIntimationForDraftParams{
		ID:       iid,
		TenantID: tid,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrIntimationNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return &IntimationContext{
		IntimationID:  row.IntimationID.String(),
		CaseID:        row.CaseID.String(),
		CourtRecordID: row.CourtRecordID.String(),
		Type:          derefString(row.IntimationType),
	}, nil
}

func (r *pgRepository) UpdateDraftContent(ctx context.Context, tx database.Tx, draftID, tenantID, content string, title *string) (*PatchResult, error) {
	did, err := parseUUID(draftID)
	if err != nil {
		return nil, err
	}
	tid, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}

	updateTitle := title != nil
	titleVal := ""
	if updateTitle {
		titleVal = *title
	}

	row, err := draftdb.New(tx).UpdateDraftContent(ctx, draftdb.UpdateDraftContentParams{
		ID:       did,
		TenantID: tid,
		Content:  textToNull(content),
		Column4:  updateTitle,
		Title:    titleVal,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrDraftNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return &PatchResult{
		ID:        row.ID.String(),
		Title:     row.Title,
		UpdatedAt: timestamptzToTime(row.UpdatedAt),
	}, nil
}

func (r *pgRepository) GetDraftDetail(ctx context.Context, tx database.Tx, tenantID, draftID string) (*DraftDetailView, error) {
	tid, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}
	did, err := parseUUID(draftID)
	if err != nil {
		return nil, err
	}

	row, err := draftdb.New(tx).GetDraftDetail(ctx, draftdb.GetDraftDetailParams{
		ID:       did,
		TenantID: tid,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrDraftNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return detailViewFromRow(row), nil
}

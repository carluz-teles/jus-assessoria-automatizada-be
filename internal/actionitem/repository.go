package actionitem

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jusassessoria/platform/internal/actionitem/actionitemdb"
	"github.com/jusassessoria/platform/lib/database"
)

// Repository is the persistence port for the Providência aggregate (docs §2/§3). Every
// method takes the caller's tx so it participates in the use case's unit of work and RLS
// scopes the reads/writes to the tenant.
type Repository interface {
	// InsertActionItem materializes one providência candidate inside the caller's tx and
	// returns it with its DB-assigned id.
	InsertActionItem(ctx context.Context, tx database.Tx, a *ActionItem) (*ActionItem, error)
	// GetActionItem loads one action_item by id, scoped to tenantID (barrier 1). A missing
	// id — or one owned by another tenant — is ErrActionItemNotFound, never (nil, nil).
	GetActionItem(ctx context.Context, tx database.Tx, tenantID, id string) (*ActionItem, error)
	// ConfirmActionItem runs the guarded tipo_status a_confirmar→confiável UPDATE. A
	// concurrent write that already moved the row out of the guarded state yields
	// ErrActionItemConflict (never nil, nil) — the use case's pre-read is what
	// distinguishes idempotent-no-op from this genuine race.
	ConfirmActionItem(ctx context.Context, tx database.Tx, tenantID, id string) (*ActionItem, error)
	// DiscardActionItem runs the guarded status→DISCARDED UPDATE. Mirrors ConfirmActionItem.
	DiscardActionItem(ctx context.Context, tx database.Tx, tenantID, id string) (*ActionItem, error)
	// DeleteReplaceableActionItems clears the SUGGESTED+a_confirmar+task_id-IS-NULL rows of
	// one intimação — the guard aditivo's destructive half, run right before re-inserting
	// the fresh candidates in the SAME tx.
	DeleteReplaceableActionItems(ctx context.Context, tx database.Tx, tenantID, intimationID string) error
	// ExistsActionItemByTipo reports whether a committed candidate already exists for
	// (tenantID, intimationID, tipo, tipoOrigem) — the dedup guard against re-analysis
	// duplicating a confiável (declarado/manual) item every run.
	ExistsActionItemByTipo(ctx context.Context, tx database.Tx, tenantID, intimationID, tipo string, tipoOrigem TipoOrigem) (bool, error)
	// LinkTask writes the reverse pointer once deadline's listener has created the task for
	// this providência (task.created carrying action_item_id): task_id + status→CONFIRMED,
	// guarded by task_id IS NULL (fatia 3, docs §2/§6). A redelivered event or a missing/
	// foreign id both yield ErrActionItemNotFound — the use case treats either as a safe no-op.
	LinkTask(ctx context.Context, tx database.Tx, tenantID, actionItemID, taskID string) (*ActionItem, error)
	// HasFiledDraftForActionItem reports whether the providência's task already produced a
	// FILED (vigente, protocolada) draft — Reclassificar's guard (fatia 5, docs §7 questão 4).
	HasFiledDraftForActionItem(ctx context.Context, tx database.Tx, tenantID, actionItemID string) (bool, error)
	// ReclassifyActionItem runs the guarded tipo/piece_profile_key override UPDATE (fatia 5):
	// tipo_origem→manual, tipo_status→confiavel, gera_peca→true, confianca→NULL. A concurrent
	// descartar that already moved the row to DISCARDED yields ErrActionItemConflict.
	ReclassifyActionItem(ctx context.Context, tx database.Tx, tenantID, id, pieceProfileKey, tipo string) (*ActionItem, error)
}

// pgRepository is the sqlc-backed Repository. It is stateless: each method binds
// actionitemdb to the tx it is given, so there is nothing to inject at construction.
type pgRepository struct{}

var _ Repository = (*pgRepository)(nil)

// NewRepository returns the Repository.
func NewRepository() Repository { return &pgRepository{} }

func (r *pgRepository) InsertActionItem(ctx context.Context, tx database.Tx, a *ActionItem) (*ActionItem, error) {
	id, err := parseUUID(a.ID)
	if err != nil {
		return nil, err
	}
	tenant, err := parseUUID(a.TenantID)
	if err != nil {
		return nil, err
	}
	intimationID, err := parseUUID(a.IntimationID)
	if err != nil {
		return nil, err
	}

	row, err := actionitemdb.New(tx).InsertActionItem(ctx, actionitemdb.InsertActionItemParams{
		ID:              id,
		TenantID:        tenant,
		IntimationID:    intimationID,
		CourtRecordID:   pgOptionalUUID(a.CourtRecordID),
		Tipo:            a.Tipo,
		GeraPeca:        a.GeraPeca,
		PieceProfileKey: textToNull(a.PieceProfileKey),
		TipoOrigem:      string(a.TipoOrigem),
		TipoStatus:      string(a.TipoStatus),
		DeadlineID:      pgOptionalUUID(a.DeadlineID),
		Confianca:       a.Confianca,
		Status:          string(a.Status),
		CreatedAt:       pgTimestamptz(a.CreatedAt),
		UpdatedAt:       pgTimestamptz(a.UpdatedAt),
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return fromRow(row), nil
}

func (r *pgRepository) GetActionItem(ctx context.Context, tx database.Tx, tenantID, id string) (*ActionItem, error) {
	itemID, err := parseUUID(id)
	if err != nil {
		return nil, err
	}
	tenant, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}

	row, err := actionitemdb.New(tx).GetActionItem(ctx, actionitemdb.GetActionItemParams{ID: itemID, TenantID: tenant})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrActionItemNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return fromRow(row), nil
}

func (r *pgRepository) ConfirmActionItem(ctx context.Context, tx database.Tx, tenantID, id string) (*ActionItem, error) {
	itemID, err := parseUUID(id)
	if err != nil {
		return nil, err
	}
	tenant, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}

	row, err := actionitemdb.New(tx).ConfirmActionItem(ctx, actionitemdb.ConfirmActionItemParams{ID: itemID, TenantID: tenant})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrActionItemConflict
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return fromRow(row), nil
}

func (r *pgRepository) DiscardActionItem(ctx context.Context, tx database.Tx, tenantID, id string) (*ActionItem, error) {
	itemID, err := parseUUID(id)
	if err != nil {
		return nil, err
	}
	tenant, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}

	row, err := actionitemdb.New(tx).DiscardActionItem(ctx, actionitemdb.DiscardActionItemParams{ID: itemID, TenantID: tenant})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrActionItemConflict
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return fromRow(row), nil
}

func (r *pgRepository) DeleteReplaceableActionItems(ctx context.Context, tx database.Tx, tenantID, intimationID string) error {
	tenant, err := parseUUID(tenantID)
	if err != nil {
		return err
	}
	iid, err := parseUUID(intimationID)
	if err != nil {
		return err
	}
	if err := actionitemdb.New(tx).DeleteReplaceableActionItems(ctx, actionitemdb.DeleteReplaceableActionItemsParams{
		TenantID:     tenant,
		IntimationID: iid,
	}); err != nil {
		return database.WrapInfra(err)
	}
	return nil
}

func (r *pgRepository) LinkTask(ctx context.Context, tx database.Tx, tenantID, actionItemID, taskID string) (*ActionItem, error) {
	itemID, err := parseUUID(actionItemID)
	if err != nil {
		return nil, err
	}
	tenant, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}
	task, err := parseUUID(taskID)
	if err != nil {
		return nil, err
	}

	row, err := actionitemdb.New(tx).LinkActionItemTask(ctx, actionitemdb.LinkActionItemTaskParams{
		ID:       itemID,
		TenantID: tenant,
		TaskID:   pgtype.UUID{Bytes: [16]byte(task), Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrActionItemNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return fromRow(row), nil
}

func (r *pgRepository) ExistsActionItemByTipo(ctx context.Context, tx database.Tx, tenantID, intimationID, tipo string, tipoOrigem TipoOrigem) (bool, error) {
	tenant, err := parseUUID(tenantID)
	if err != nil {
		return false, err
	}
	iid, err := parseUUID(intimationID)
	if err != nil {
		return false, err
	}
	exists, err := actionitemdb.New(tx).ExistsActionItemByTipo(ctx, actionitemdb.ExistsActionItemByTipoParams{
		TenantID:     tenant,
		IntimationID: iid,
		Tipo:         tipo,
		TipoOrigem:   string(tipoOrigem),
	})
	if err != nil {
		return false, database.WrapInfra(err)
	}
	return exists, nil
}

func (r *pgRepository) HasFiledDraftForActionItem(ctx context.Context, tx database.Tx, tenantID, actionItemID string) (bool, error) {
	// ActionItemID binds against task.action_item_id (nullable, migration 0079), so sqlc
	// infers a pgtype.UUID param — not the plain uuid.UUID a required/PK column would get.
	itemID, err := parseUUID(actionItemID)
	if err != nil {
		return false, err
	}
	tenant, err := parseUUID(tenantID)
	if err != nil {
		return false, err
	}
	exists, err := actionitemdb.New(tx).HasFiledDraftForActionItem(ctx, actionitemdb.HasFiledDraftForActionItemParams{
		ActionItemID: pgtype.UUID{Bytes: [16]byte(itemID), Valid: true},
		TenantID:     tenant,
	})
	if err != nil {
		return false, database.WrapInfra(err)
	}
	return exists, nil
}

func (r *pgRepository) ReclassifyActionItem(ctx context.Context, tx database.Tx, tenantID, id, pieceProfileKey, tipo string) (*ActionItem, error) {
	itemID, err := parseUUID(id)
	if err != nil {
		return nil, err
	}
	tenant, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}

	row, err := actionitemdb.New(tx).ReclassifyActionItem(ctx, actionitemdb.ReclassifyActionItemParams{
		ID:              itemID,
		TenantID:        tenant,
		PieceProfileKey: textToNull(pieceProfileKey),
		Tipo:            tipo,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrActionItemConflict
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return fromRow(row), nil
}

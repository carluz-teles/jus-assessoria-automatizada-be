package acquisition

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jusassessoria/platform/internal/acquisition/acquisitiondb"
	"github.com/jusassessoria/platform/lib/database"
)

// Repository is the persistence port the use case depends on (it never sees the
// concrete impl). The two upsert-path methods receive the caller's transaction —
// the use case owns the boundary, the repo only participates — so the row read
// and its upsert share one tx and the outbox write commits with them. List is a
// plain read on the pool, scoped by tenant_id (isolation barrier 1).
type Repository interface {
	GetBySource(ctx context.Context, tx database.Tx, tenantID, source string) (*Integration, error)
	Upsert(ctx context.Context, tx database.Tx, tenantID, source string, scope Scope) (*Integration, error)
	List(ctx context.Context, tenantID string) ([]*Integration, error)
}

// pgRepository is the sqlc-backed implementation. q is bound to the pool for
// reads; the tx-taking writes rebind the generated queries to the passed tx.
type pgRepository struct {
	q *acquisitiondb.Queries
}

var _ Repository = (*pgRepository)(nil)

// NewRepository binds the generated queries to pool (used for reads). Inject a
// *pgxpool.Pool in production; both it and a mock satisfy acquisitiondb.DBTX.
func NewRepository(pool acquisitiondb.DBTX) Repository {
	return &pgRepository{q: acquisitiondb.New(pool)}
}

// GetBySource loads the current integration for (tenant, source) inside the
// caller's tx. A missing row is the typed ErrIntegrationNotFound, never
// (nil, nil), so the use case can branch on "first activation".
func (r *pgRepository) GetBySource(ctx context.Context, tx database.Tx, tenantID, source string) (*Integration, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	row, err := acquisitiondb.New(tx).GetIntegrationBySource(ctx, acquisitiondb.GetIntegrationBySourceParams{
		TenantID: tid,
		Source:   source,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrIntegrationNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return integrationToEntity(row)
}

// Upsert activates or re-activates (tenant, source) inside the caller's tx,
// setting the scope and forcing status ACTIVE. RETURNING always yields a row, so
// there is no not-found branch. credential_ref is never written here.
func (r *pgRepository) Upsert(ctx context.Context, tx database.Tx, tenantID, source string, scope Scope) (*Integration, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	raw, err := encodeScope(scope)
	if err != nil {
		return nil, err
	}
	row, err := acquisitiondb.New(tx).UpsertIntegration(ctx, acquisitiondb.UpsertIntegrationParams{
		TenantID: tid,
		Source:   source,
		Scope:    raw,
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return integrationToEntity(row)
}

// List returns all of the tenant's integrations, oldest first, filtered by
// tenant_id on the pool. Read models never assemble an aggregate; this returns
// the mapped entities the read handler renders to a view.
func (r *pgRepository) List(ctx context.Context, tenantID string) ([]*Integration, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	rows, err := r.q.ListIntegrations(ctx, tid)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	out := make([]*Integration, 0, len(rows))
	for _, row := range rows {
		ent, err := integrationToEntity(row)
		if err != nil {
			return nil, err
		}
		out = append(out, ent)
	}
	return out, nil
}

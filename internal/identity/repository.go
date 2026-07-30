package identity

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jusassessoria/platform/internal/identity/identitydb"
	"github.com/jusassessoria/platform/lib/database"
)

// Repository is the persistence port the use cases depend on (they never see the
// concrete impl). Writes receive the caller's transaction — the use case owns
// the boundary, the repo only participates (docs §4b.1); reads run on the pool.
type Repository interface {
	UpsertTenant(ctx context.Context, tx database.Tx, clerkOrgID, name string) (*Tenant, error)
	FindTenantByClerkOrg(ctx context.Context, clerkOrgID string) (*Tenant, error)
	UpsertUser(ctx context.Context, tx database.Tx, clerkUserID, tenantID, email, name, phone string, role Role) (*AppUser, error)
	FindUserByClerkUser(ctx context.Context, clerkUserID string) (*AppUser, error)
}

// pgRepository is the sqlc-backed implementation. q is bound to the pool for
// reads; writes rebind the generated queries to the passed transaction.
type pgRepository struct {
	q *identitydb.Queries
}

var _ Repository = (*pgRepository)(nil)

// NewRepository binds the generated queries to pool (used for reads). Inject a
// *pgxpool.Pool in production; both it and a mock satisfy identitydb.DBTX.
func NewRepository(pool identitydb.DBTX) Repository {
	return &pgRepository{q: identitydb.New(pool)}
}

// UpsertTenant provisions or refreshes a tenant inside the caller's tx. RETURNING
// always yields a row, so there is no not-found branch here.
func (r *pgRepository) UpsertTenant(ctx context.Context, tx database.Tx, clerkOrgID, name string) (*Tenant, error) {
	row, err := identitydb.New(tx).UpsertTenant(ctx, identitydb.UpsertTenantParams{
		ClerkOrgID: clerkOrgID,
		Name:       name,
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return tenantToEntity(row), nil
}

func (r *pgRepository) FindTenantByClerkOrg(ctx context.Context, clerkOrgID string) (*Tenant, error) {
	row, err := r.q.GetTenantByClerkOrg(ctx, clerkOrgID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrTenantNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return tenantToEntity(row), nil
}

// UpsertUser provisions or refreshes an app_user inside the caller's tx. tenantID
// is the internal uuid (a string on the entity), parsed back to uuid.UUID here.
func (r *pgRepository) UpsertUser(ctx context.Context, tx database.Tx, clerkUserID, tenantID, email, name, phone string, role Role) (*AppUser, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	row, err := identitydb.New(tx).UpsertUser(ctx, identitydb.UpsertUserParams{
		ClerkUserID: clerkUserID,
		TenantID:    tid,
		Email:       email,
		Name:        textToNull(name),
		Phone:       textToNull(phone),
		Role:        string(role),
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return userToEntity(row), nil
}

func (r *pgRepository) FindUserByClerkUser(ctx context.Context, clerkUserID string) (*AppUser, error) {
	row, err := r.q.GetUserByClerkUser(ctx, clerkUserID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return userToEntity(row), nil
}

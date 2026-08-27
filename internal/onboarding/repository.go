package onboarding

import (
	"context"

	"github.com/google/uuid"

	"github.com/jusassessoria/platform/internal/onboarding/onboardingdb"
	"github.com/jusassessoria/platform/lib/database"
)

// Repository is the persistence port the use case depends on. GetProgress runs
// on the pool (a screen read, no tx); Dismiss participates in the caller's tx
// (the use case owns the transaction boundary, docs §4b.1).
type Repository interface {
	// GetProgress reads the tenant-wide activation Steps plus the caller's own
	// dismissal timestamp in one round-trip (EXISTS subqueries). tenantID scopes
	// the Steps; appUserID scopes DismissedAt (per-user, not per-tenant).
	GetProgress(ctx context.Context, tenantID, appUserID string) (Progress, error)
	// Dismiss upserts the caller's dismissal timestamp inside the caller's tx.
	// Idempotent: a repeat dismiss just restamps dismissed_at, never an error.
	Dismiss(ctx context.Context, tx database.Tx, tenantID, appUserID string) error
}

// pgRepository is the sqlc-backed implementation. q is bound to the pool for
// the read; the write rebinds the generated queries to the passed transaction.
type pgRepository struct {
	q *onboardingdb.Queries
}

var _ Repository = (*pgRepository)(nil)

// NewRepository binds the generated queries to pool (used for GetProgress).
// Inject a *pgxpool.Pool in production; both it and a mock satisfy
// onboardingdb.DBTX.
func NewRepository(pool onboardingdb.DBTX) Repository {
	return &pgRepository{q: onboardingdb.New(pool)}
}

// GetProgress reads on the pool (a screen read, no tx). tenantID/appUserID are
// internal uuids (strings at the entity boundary), parsed back here — both
// always come from the verified principal, so a parse failure is an infra
// fault, never a client error.
func (r *pgRepository) GetProgress(ctx context.Context, tenantID, appUserID string) (Progress, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return Progress{}, database.WrapInfra(err)
	}
	uid, err := uuid.Parse(appUserID)
	if err != nil {
		return Progress{}, database.WrapInfra(err)
	}

	row, err := r.q.GetProgress(ctx, onboardingdb.GetProgressParams{
		TenantID:  tid,
		AppUserID: uid,
	})
	if err != nil {
		return Progress{}, database.WrapInfra(err)
	}
	return progressToEntity(row), nil
}

// Dismiss writes inside the caller's tx. ON CONFLICT (app_user_id) makes it an
// upsert, so there is no not-found branch — a first dismiss inserts, a repeat
// just restamps dismissed_at.
func (r *pgRepository) Dismiss(ctx context.Context, tx database.Tx, tenantID, appUserID string) error {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return database.WrapInfra(err)
	}
	uid, err := uuid.Parse(appUserID)
	if err != nil {
		return database.WrapInfra(err)
	}

	if err := onboardingdb.New(tx).Dismiss(ctx, onboardingdb.DismissParams{
		AppUserID: uid,
		TenantID:  tid,
	}); err != nil {
		return database.WrapInfra(err)
	}
	return nil
}

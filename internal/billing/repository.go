package billing

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jusassessoria/platform/internal/billing/billingdb"
	"github.com/jusassessoria/platform/lib/database"
)

// UpsertParams is the write DTO for a full subscription projection (created/
// updated). It is the domain's own shape — the repo translates it to the sqlc
// params at the boundary — so the use case never touches uuid.UUID or pgtype.*.
type UpsertParams struct {
	TenantID             string
	StripeCustomerID     string
	StripeSubscriptionID string
	Status               Status
	Plan                 string
	CurrentPeriodEnd     time.Time
	ActiveProcessLimit   int
}

// Repository is the persistence port the use cases depend on (they never see the
// concrete impl). Writes receive the caller's transaction — the use case owns the
// boundary, the repo only participates (docs §4b.1); reads run on the pool.
type Repository interface {
	// UpsertSubscription projects the full subscription state inside the caller's
	// tx, keyed on tenant_id (one row per tenant). RETURNING always yields a row,
	// so there is no not-found branch.
	UpsertSubscription(ctx context.Context, tx database.Tx, params UpsertParams) (*Subscription, error)
	// UpdateSubscriptionStatus flips only the lifecycle status by tenant id inside
	// the caller's tx, leaving the projected entitlement untouched. A no-row result
	// (a status event racing ahead of the create) is ErrSubscriptionNotFound.
	UpdateSubscriptionStatus(ctx context.Context, tx database.Tx, tenantID string, status Status) (*Subscription, error)
	// FindByStripeCustomer reads the subscription (and thus its tenant) by Stripe
	// customer id, on the pool. A missing row is ErrSubscriptionNotFound, never
	// (nil, nil).
	FindByStripeCustomer(ctx context.Context, stripeCustomerID string) (*Subscription, error)
	// FindByTenant reads the tenant's own subscription projection on the pool —
	// backs the read-model endpoint and the checkout/portal flows. A missing row is
	// ErrSubscriptionNotFound, never (nil, nil).
	FindByTenant(ctx context.Context, tenantID string) (*Subscription, error)
}

// pgRepository is the sqlc-backed implementation. q is bound to the pool for
// reads; writes rebind the generated queries to the passed transaction.
type pgRepository struct {
	q *billingdb.Queries
}

var _ Repository = (*pgRepository)(nil)

// NewRepository binds the generated queries to pool (used for reads). Inject a
// *pgxpool.Pool in production; both it and a mock satisfy billingdb.DBTX.
func NewRepository(pool billingdb.DBTX) Repository {
	return &pgRepository{q: billingdb.New(pool)}
}

// UpsertSubscription projects the subscription inside the caller's tx. tenantID is
// the internal uuid (a string on the DTO), parsed back to uuid.UUID here; the
// optional text/limit fields are written as SQL NULL when zero.
func (r *pgRepository) UpsertSubscription(ctx context.Context, tx database.Tx, params UpsertParams) (*Subscription, error) {
	tid, err := uuid.Parse(params.TenantID)
	if err != nil {
		return nil, database.WrapInfra(err)
	}

	row, err := billingdb.New(tx).UpsertSubscription(ctx, billingdb.UpsertSubscriptionParams{
		TenantID:             tid,
		StripeCustomerID:     textToNull(params.StripeCustomerID),
		StripeSubscriptionID: textToNull(params.StripeSubscriptionID),
		Status:               string(params.Status),
		Plan:                 textToNull(params.Plan),
		CurrentPeriodEnd:     timeToTimestamptz(params.CurrentPeriodEnd),
		ActiveProcessLimit:   intToNull(params.ActiveProcessLimit),
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return subscriptionToEntity(row), nil
}

// UpdateSubscriptionStatus flips the status by tenant id inside the caller's tx.
// A no-row result (RETURNING empty) means no subscription exists yet for the
// tenant — ErrSubscriptionNotFound, so the caller lets Stripe retry.
func (r *pgRepository) UpdateSubscriptionStatus(ctx context.Context, tx database.Tx, tenantID string, status Status) (*Subscription, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, database.WrapInfra(err)
	}

	row, err := billingdb.New(tx).UpdateSubscriptionStatus(ctx, billingdb.UpdateSubscriptionStatusParams{
		TenantID: tid,
		Status:   string(status),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrSubscriptionNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return subscriptionToEntity(row), nil
}

// FindByTenant reads the tenant's subscription on the pool. A missing row is the
// typed ErrSubscriptionNotFound (the tenant never checked out), never (nil, nil).
func (r *pgRepository) FindByTenant(ctx context.Context, tenantID string) (*Subscription, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, database.WrapInfra(err)
	}

	row, err := r.q.FindByTenant(ctx, tid)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrSubscriptionNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return subscriptionToEntity(row), nil
}

// FindByStripeCustomer reads on the pool (a resolution read, no tx). A missing row
// is the typed ErrSubscriptionNotFound — the caller distinguishes "no subscription
// for this customer yet" from an infra fault by that sentinel, never (nil, nil).
func (r *pgRepository) FindByStripeCustomer(ctx context.Context, stripeCustomerID string) (*Subscription, error) {
	row, err := r.q.FindByStripeCustomer(ctx, textToNull(stripeCustomerID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrSubscriptionNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return subscriptionToEntity(row), nil
}

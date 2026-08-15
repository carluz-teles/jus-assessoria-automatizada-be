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
// PlanID is the local plan resolved from the Stripe price id (nil only if the
// caller has not resolved one — applySubscription always sets it, failing loud
// otherwise). CustomPricePerProcessCents/TrialEndsAt are left nil on the webhook
// path in this fatia: the upsert query preserves any existing value instead of
// clobbering it (see queries/billing.sql).
type UpsertParams struct {
	TenantID                   string
	StripeCustomerID           string
	StripeSubscriptionID       string
	Status                     Status
	Plan                       string
	CurrentPeriodEnd           time.Time
	ActiveProcessLimit         int
	PlanID                     *string
	CustomPricePerProcessCents *int
	TrialEndsAt                *time.Time
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
	// FindPlanByID resolves a plan by its own id — the entitlement adapter's
	// lookup for the plan a subscription references. Deliberately not filtered by
	// active (see queries/billing.sql). A missing row is ErrPlanNotFound, never
	// (nil, nil).
	FindPlanByID(ctx context.Context, tx database.Tx, planID string) (*Plan, error)
	// FindPlanByStripePriceID resolves the local plan behind a Stripe price id,
	// inside the caller's tx — applySubscription (domain.go) runs this as a local
	// read now that the catalog lives in this BE, not a Stripe HTTP call. A missing
	// row is ErrPlanNotFound, never (nil, nil).
	FindPlanByStripePriceID(ctx context.Context, tx database.Tx, priceID string) (*Plan, error)
	// FindPlanByCode resolves a plan by its stable code (used to resolve the plan a
	// trial starts on, fatia 2). A missing row is ErrPlanNotFound, never (nil, nil).
	FindPlanByCode(ctx context.Context, tx database.Tx, code string) (*Plan, error)
	// ListActivePlans reads the active plan catalog, ordered by the process band it
	// prices.
	ListActivePlans(ctx context.Context, tx database.Tx) ([]*Plan, error)
	// FindDefaultTrialPolicy reads the single ACTIVE default trial policy. A
	// missing row is ErrNoDefaultTrialPolicy, never (nil, nil) — the catalog must
	// always have exactly one for a fresh signup to start a trial.
	FindDefaultTrialPolicy(ctx context.Context, tx database.Tx) (*TrialPolicy, error)
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
		TenantID:                   tid,
		StripeCustomerID:           textToNull(params.StripeCustomerID),
		StripeSubscriptionID:       textToNull(params.StripeSubscriptionID),
		Status:                     string(params.Status),
		Plan:                       textToNull(params.Plan),
		CurrentPeriodEnd:           timeToTimestamptz(params.CurrentPeriodEnd),
		ActiveProcessLimit:         intToNull(params.ActiveProcessLimit),
		PlanID:                     strPtrToUUID(params.PlanID),
		CustomPricePerProcessCents: intPtrToNull32(params.CustomPricePerProcessCents),
		TrialEndsAt:                timePtrToTimestamptz(params.TrialEndsAt),
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

// queries binds to tx when the caller passes one (applySubscription's webhook
// tx) or falls back to the pool otherwise (a standalone read, e.g. the
// entitlement adapter's plan lookup, which runs outside any transaction) — the
// same read/write split FindByTenant vs UpsertSubscription already draw, unified
// here because the plan/trial-policy queries below are read from both contexts.
func (r *pgRepository) queries(tx database.Tx) *billingdb.Queries {
	if tx == nil {
		return r.q
	}
	return billingdb.New(tx)
}

// FindPlanByID resolves a plan by its own id. A missing row is the typed
// ErrPlanNotFound, never (nil, nil).
func (r *pgRepository) FindPlanByID(ctx context.Context, tx database.Tx, planID string) (*Plan, error) {
	id, err := uuid.Parse(planID)
	if err != nil {
		return nil, database.WrapInfra(err)
	}

	row, err := r.queries(tx).FindPlanByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrPlanNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return planToEntity(row), nil
}

// FindPlanByStripePriceID resolves the local plan for a Stripe price id. A
// missing row is the typed ErrPlanNotFound, never (nil, nil).
func (r *pgRepository) FindPlanByStripePriceID(ctx context.Context, tx database.Tx, priceID string) (*Plan, error) {
	row, err := r.queries(tx).FindPlanByStripePriceID(ctx, textToNull(priceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrPlanNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return planToEntity(row), nil
}

// FindPlanByCode resolves a plan by its stable code. A missing row is the typed
// ErrPlanNotFound, never (nil, nil).
func (r *pgRepository) FindPlanByCode(ctx context.Context, tx database.Tx, code string) (*Plan, error) {
	row, err := r.queries(tx).FindPlanByCode(ctx, code)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrPlanNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return planToEntity(row), nil
}

// ListActivePlans reads the active plan catalog, ordered by the process band
// each plan prices.
func (r *pgRepository) ListActivePlans(ctx context.Context, tx database.Tx) ([]*Plan, error) {
	rows, err := r.queries(tx).ListActivePlans(ctx)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	plans := make([]*Plan, 0, len(rows))
	for _, row := range rows {
		plans = append(plans, planToEntity(row))
	}
	return plans, nil
}

// FindDefaultTrialPolicy reads the single ACTIVE default trial policy. A missing
// row is the typed ErrNoDefaultTrialPolicy, never (nil, nil).
func (r *pgRepository) FindDefaultTrialPolicy(ctx context.Context, tx database.Tx) (*TrialPolicy, error) {
	row, err := r.queries(tx).FindDefaultTrialPolicy(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoDefaultTrialPolicy
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return trialPolicyToEntity(row), nil
}

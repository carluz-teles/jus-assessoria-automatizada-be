package billing

import (
	"context"
	"errors"
	"testing"

	"github.com/jusassessoria/platform/lib/database"
)

// entitlementRepo is a minimal Repository double for the adapter: FindByTenant
// and FindPlanByID are the only two methods a live path can reach, so the rest
// nil-panic if a path unexpectedly reaches them. (The package's richer mockRepo
// lives in domain_test.go; this keeps the adapter test self-contained around the
// two methods it uses.)
type entitlementRepo struct {
	sub     *Subscription
	err     error
	plan    *Plan
	planErr error
}

func (r *entitlementRepo) FindByTenant(_ context.Context, _ string) (*Subscription, error) {
	return r.sub, r.err
}

func (r *entitlementRepo) UpsertSubscription(context.Context, database.Tx, UpsertParams) (*Subscription, error) {
	panic("unexpected UpsertSubscription")
}

func (r *entitlementRepo) UpdateSubscriptionStatus(context.Context, database.Tx, string, Status) (*Subscription, error) {
	panic("unexpected UpdateSubscriptionStatus")
}

func (r *entitlementRepo) FindByStripeCustomer(context.Context, string) (*Subscription, error) {
	panic("unexpected FindByStripeCustomer")
}

func (r *entitlementRepo) FindPlanByID(_ context.Context, _ database.Tx, _ string) (*Plan, error) {
	return r.plan, r.planErr
}

func (r *entitlementRepo) FindPlanByStripePriceID(context.Context, database.Tx, string) (*Plan, error) {
	panic("unexpected FindPlanByStripePriceID")
}

func (r *entitlementRepo) FindPlanByCode(context.Context, database.Tx, string) (*Plan, error) {
	panic("unexpected FindPlanByCode")
}

func (r *entitlementRepo) ListActivePlans(context.Context, database.Tx) ([]*Plan, error) {
	panic("unexpected ListActivePlans")
}

func (r *entitlementRepo) FindDefaultTrialPolicy(context.Context, database.Tx) (*TrialPolicy, error) {
	panic("unexpected FindDefaultTrialPolicy")
}

// AC3 + success + error propagation + the Status gap this fatia closes: the
// adapter maps ErrSubscriptionNotFound to a fail-closed limit of 0, revokes the
// entitlement (limit 0) on canceled/past_due REGARDLESS of the stored
// active_process_limit (the gap: this used to ignore Status entirely), resolves
// active/trialing through the plan via effectiveEntitlement, and propagates any
// other (infra) error unchanged — never folding it to 0.
func TestEntitlementAdapter_ActiveProcessLimit(t *testing.T) {
	t.Parallel()

	infraDown := errors.New("db unreachable")
	planID := "plan-growth"
	growthPlan := &Plan{ID: planID, MaxProcesses: intPtr(500), PricePerProcessCents: 35}
	enterprisePlan := &Plan{ID: planID, MaxProcesses: nil, PricePerProcessCents: 15}

	tests := []struct {
		name      string
		repo      *entitlementRepo
		wantLimit int
		wantErr   error // sentinel to match with errors.Is; nil = expect no error
	}{
		{
			name:      "no subscription is fail-closed limit 0",
			repo:      &entitlementRepo{err: ErrSubscriptionNotFound},
			wantLimit: 0,
			wantErr:   nil,
		},
		{
			name: "canceled subscription is fail-closed limit 0 despite a stored limit",
			repo: &entitlementRepo{sub: &Subscription{
				Status: StatusCanceled, ActiveProcessLimit: 500, PlanID: &planID,
			}},
			wantLimit: 0,
			wantErr:   nil,
		},
		{
			name: "past_due subscription is fail-closed limit 0 despite a stored limit",
			repo: &entitlementRepo{sub: &Subscription{
				Status: StatusPastDue, ActiveProcessLimit: 500, PlanID: &planID,
			}},
			wantLimit: 0,
			wantErr:   nil,
		},
		{
			name: "active subscription resolves its limit via the plan",
			repo: &entitlementRepo{
				sub:  &Subscription{Status: StatusActive, PlanID: &planID},
				plan: growthPlan,
			},
			wantLimit: 500,
			wantErr:   nil,
		},
		{
			name: "trialing subscription resolves its limit via the plan",
			repo: &entitlementRepo{
				sub:  &Subscription{Status: StatusTrialing, PlanID: &planID},
				plan: growthPlan,
			},
			wantLimit: 500,
			wantErr:   nil,
		},
		{
			name: "active subscription on the no-ceiling (Enterprise) plan",
			repo: &entitlementRepo{
				sub:  &Subscription{Status: StatusActive, PlanID: &planID},
				plan: enterprisePlan,
			},
			wantLimit: unlimitedActiveProcesses,
			wantErr:   nil,
		},
		{
			name: "active subscription with no plan_id yet (pre-migration row) fails closed",
			repo: &entitlementRepo{
				sub: &Subscription{Status: StatusActive, PlanID: nil},
			},
			wantLimit: 0,
			wantErr:   nil,
		},
		{
			name:      "infra error propagates, never folded to 0",
			repo:      &entitlementRepo{err: infraDown},
			wantLimit: 0,
			wantErr:   infraDown,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			adapter := NewEntitlementAdapter(tt.repo)
			got, err := adapter.ActiveProcessLimit(context.Background(), "tenant-1")

			if tt.wantErr == nil && err != nil {
				t.Fatalf("ActiveProcessLimit() error = %v, want nil", err)
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("ActiveProcessLimit() error = %v, want %v", err, tt.wantErr)
			}
			if got != tt.wantLimit {
				t.Fatalf("ActiveProcessLimit() = %d, want %d", got, tt.wantLimit)
			}
		})
	}
}

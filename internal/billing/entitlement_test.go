package billing

import (
	"context"
	"errors"
	"testing"

	"github.com/jusassessoria/platform/lib/database"
)

// entitlementRepo is a minimal Repository double for the adapter: only FindByTenant
// is exercised, so the other three methods nil-panic if a path unexpectedly reaches
// them. (The package's richer mockRepo lives in domain_test.go; this keeps the
// adapter test self-contained around the one method it uses.)
type entitlementRepo struct {
	sub *Subscription
	err error
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

// AC3 + success + error propagation: the adapter maps ErrSubscriptionNotFound to a
// fail-closed limit of 0, passes a real subscription's limit through, and propagates
// any other (infra) error unchanged — never folding it to 0.
func TestEntitlementAdapter_ActiveProcessLimit(t *testing.T) {
	t.Parallel()

	infraDown := errors.New("db unreachable")

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
			name:      "active subscription passes its limit through",
			repo:      &entitlementRepo{sub: &Subscription{ActiveProcessLimit: 25}},
			wantLimit: 25,
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

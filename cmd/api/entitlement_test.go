package main

import (
	"context"
	"math"
	"testing"

	"github.com/jusassessoria/platform/internal/billing"
	"github.com/jusassessoria/platform/lib/config"
)

// TestResolveEntitlementChecker_GateDisabled asserts that with
// BillingGateEnabled false (the default while plan pricing is undecided),
// resolveEntitlementChecker wires an unlimited checker: ActiveProcessLimit
// returns math.MaxInt regardless of any subscription state (there is none
// here — repo is backed by a nil pool, which would fail loudly if the real
// billing.EntitlementAdapter were ever consulted on this path).
func TestResolveEntitlementChecker_GateDisabled(t *testing.T) {
	cfg := config.Config{BillingGateEnabled: false}
	repo := billing.NewRepository(nil)

	checker := resolveEntitlementChecker(cfg, repo)

	limit, err := checker.ActiveProcessLimit(context.Background(), "tenant-1")
	if err != nil {
		t.Fatalf("ActiveProcessLimit() error = %v, want nil", err)
	}
	if limit != math.MaxInt {
		t.Fatalf("ActiveProcessLimit() = %d, want math.MaxInt (unlimited)", limit)
	}
}

// TestResolveEntitlementChecker_GateEnabled asserts that flipping
// BillingGateEnabled true restores the real billing.EntitlementAdapter —
// the enforcement path this flag temporarily bypasses, not removes.
func TestResolveEntitlementChecker_GateEnabled(t *testing.T) {
	cfg := config.Config{BillingGateEnabled: true}
	repo := billing.NewRepository(nil)

	checker := resolveEntitlementChecker(cfg, repo)

	if _, ok := checker.(*billing.EntitlementAdapter); !ok {
		t.Fatalf("resolveEntitlementChecker() = %T, want *billing.EntitlementAdapter", checker)
	}
}

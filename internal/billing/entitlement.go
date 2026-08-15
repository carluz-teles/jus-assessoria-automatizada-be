package billing

import (
	"context"
	"errors"
	"time"
)

// EntitlementAdapter reads a tenant's v0 entitlement (active_process_limit) over
// this slice's Repository. It exists so a consumer in another slice (acquisition)
// can gate on the limit WITHOUT the two slices importing each other: acquisition
// defines the port it needs (EntitlementChecker), billing supplies this concrete
// reader, and cmd/worker-ingestao injects it — the same consumer-defines-the-port
// rule the docs apply to routes, here applied to a synchronous cross-slice read.
type EntitlementAdapter struct {
	repo Repository
	// now is the reference clock a trialing subscription's TrialEndsAt is
	// compared against; it defaults to time.Now and is overridable in tests via
	// WithEntitlementClock (mirroring UseCase.now/WithClock in domain.go).
	now func() time.Time
}

// EntitlementOption configures optional EntitlementAdapter collaborators.
type EntitlementOption func(*EntitlementAdapter)

// WithEntitlementClock overrides the reference clock ActiveProcessLimit compares a
// trialing subscription's TrialEndsAt against. Production leaves the default
// (time.Now); tests pin it to assert deterministically.
func WithEntitlementClock(now func() time.Time) EntitlementOption {
	return func(a *EntitlementAdapter) { a.now = now }
}

// NewEntitlementAdapter builds the adapter over the billing repository. FindByTenant
// reads on the pool (no tx), so a *pgxpool.Pool-backed repo is all it needs.
func NewEntitlementAdapter(repo Repository, opts ...EntitlementOption) *EntitlementAdapter {
	a := &EntitlementAdapter{repo: repo, now: time.Now}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// ActiveProcessLimit returns the tenant's ceiling on ACTIVE processes.
//
// Fail-closed cases, all limit 0:
//   - no subscription at all (ErrSubscriptionNotFound) — the tenant never
//     checked out;
//   - a canceled or past_due subscription — a lapsed/failed payment revokes the
//     entitlement immediately, it does not linger at its last-known limit. This
//     was the gap this fatia closes: the method used to return the STORED
//     active_process_limit regardless of Status, so a canceled tenant kept its
//     entitlement until some other path noticed.
//   - a trialing subscription whose TrialEndsAt has already passed — Status
//     alone does not flip to canceled/past_due when a trial lapses without
//     conversion (that automatic transition is a separate, undecided product
//     question), so ActiveProcessLimit checks the deadline itself here rather
//     than trusting a stale Status. A nil TrialEndsAt on a trialing
//     subscription should not happen (trial provisioning always sets it), but
//     is treated as "no deadline yet" (not expired) rather than panicking.
//
// An active (or still-within-trial) subscription resolves its limit via
// effectiveEntitlement, the sole plan+subscription+override combinator — this
// method never re-derives that rule. Any error other than
// ErrSubscriptionNotFound (infra, or the local plan the subscription
// references having vanished) propagates unchanged so the caller fails the
// item rather than silently treating it as limit 0.
func (a *EntitlementAdapter) ActiveProcessLimit(ctx context.Context, tenantID string) (int, error) {
	sub, err := a.repo.FindByTenant(ctx, tenantID)
	if errors.Is(err, ErrSubscriptionNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}

	switch {
	case sub.Status == StatusCanceled, sub.Status == StatusPastDue:
		return 0, nil
	case sub.Status == StatusTrialing && sub.TrialEndsAt != nil && sub.TrialEndsAt.Before(a.now()):
		return 0, nil
	case sub.Status == StatusActive, sub.Status == StatusTrialing:
		return a.resolveLimit(ctx, sub)
	default:
		// Status is validated on write (Status.Valid()); an unrecognized value
		// here would be a data fault, not a product state — fail closed rather
		// than guess at an entitlement.
		return 0, nil
	}
}

// resolveLimit reads the subscription's plan and resolves the limit through
// effectiveEntitlement. sub.PlanID is nil for a row projected before migration
// 0037 (the next webhook re-projects it); until then there is no plan to derive
// a limit from, so this fails closed rather than guessing. tx is nil — this read
// runs outside any transaction, on the pool (see pgRepository.queries).
func (a *EntitlementAdapter) resolveLimit(ctx context.Context, sub *Subscription) (int, error) {
	if sub.PlanID == nil {
		return 0, nil
	}

	plan, err := a.repo.FindPlanByID(ctx, nil, *sub.PlanID)
	if err != nil {
		return 0, err
	}

	limit, _ := effectiveEntitlement(sub, plan)
	return limit, nil
}

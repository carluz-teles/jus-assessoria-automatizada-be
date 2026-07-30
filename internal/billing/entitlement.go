package billing

import (
	"context"
	"errors"
)

// EntitlementAdapter reads a tenant's v0 entitlement (active_process_limit) over
// this slice's Repository. It exists so a consumer in another slice (acquisition)
// can gate on the limit WITHOUT the two slices importing each other: acquisition
// defines the port it needs (EntitlementChecker), billing supplies this concrete
// reader, and cmd/worker-ingestao injects it — the same consumer-defines-the-port
// rule the docs apply to routes, here applied to a synchronous cross-slice read.
type EntitlementAdapter struct {
	repo Repository
}

// NewEntitlementAdapter builds the adapter over the billing repository. FindByTenant
// reads on the pool (no tx), so a *pgxpool.Pool-backed repo is all it needs.
func NewEntitlementAdapter(repo Repository) *EntitlementAdapter {
	return &EntitlementAdapter{repo: repo}
}

// ActiveProcessLimit returns the tenant's ceiling on ACTIVE processes. A tenant that
// never checked out (ErrSubscriptionNotFound) has NO entitlement, so its limit is 0
// — fail-closed: block every new process until it subscribes. Any other error is an
// infra fault and propagates unchanged (the caller fails the item, never treating it
// as limit 0). On success the projected active_process_limit is returned as-is.
func (a *EntitlementAdapter) ActiveProcessLimit(ctx context.Context, tenantID string) (int, error) {
	sub, err := a.repo.FindByTenant(ctx, tenantID)
	if errors.Is(err, ErrSubscriptionNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return sub.ActiveProcessLimit, nil
}

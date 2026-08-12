package acquisition

import (
	"context"
	"errors"
	"slices"

	"github.com/google/uuid"

	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// publisher is the narrow outbox port the use case needs — the producer half of
// the transactional outbox. *events.Outbox satisfies it structurally.
type publisher interface {
	Publish(ctx context.Context, tx database.Tx, ev events.Event) error
	// PublishBatch emits many events in one round-trip — used where a write fans out
	// hundreds of observed events under the advisory lock (a sync window's
	// court_record_observed set), so the lock is held for one insert, not N.
	PublishBatch(ctx context.Context, tx database.Tx, evs []events.Event) error
}

// UseCase carries the acquisition use cases. It depends on the Repository
// interface, the outbox publisher and the UnitOfWork — never on the concrete pg
// implementation. It has exactly two methods: ActivateIntegration and
// ListIntegrations.
type UseCase struct {
	repo    Repository
	outbox  publisher
	uow     database.UnitOfWork
	checker EntitlementChecker
}

// useCaseOption tunes a UseCase at construction — the same option-pattern
// SyncUseCase uses for its own checker (sync.go). It cannot share the name
// WithEntitlementChecker (a Go package cannot declare two funcs with that name
// returning different option types), so this one is named for what it gates.
type useCaseOption func(*UseCase)

// WithActivationEntitlementChecker injects the billing entitlement port
// ActivateIntegration consults, at the edge, before activating a new source —
// the ERD's "entitlements na borda" gate. Without it the use case imposes no
// ceiling (unlimitedEntitlement, the same default sync.go falls back to);
// cmd/api wires the real billing adapter here.
func WithActivationEntitlementChecker(c EntitlementChecker) useCaseOption {
	return func(uc *UseCase) { uc.checker = c }
}

// NewUseCase wires the acquisition use cases to their dependencies.
func NewUseCase(repo Repository, outbox publisher, uow database.UnitOfWork, opts ...useCaseOption) *UseCase {
	uc := &UseCase{repo: repo, outbox: outbox, uow: uow, checker: unlimitedEntitlement{}}
	for _, opt := range opts {
		opt(uc)
	}
	return uc
}

// ActivateIntegration activates every requested source under one shared scope,
// atomically: one integration row per source is upserted and an
// integration_activated event is written to the outbox in the SAME transaction.
// If anything fails — an upsert or a publish — the whole batch rolls back, so a
// request that names two sources never persists one row and loses the other.
//
// The event fires only when activation changed something: a brand-new source, a
// changed scope, or a re-activation of a source that was not ACTIVE. Re-posting
// an identical, already-active integration is a no-op event-wise (the row's
// updated_at still advances), so a retry does not spam consumers.
//
// Callers pass validated input (the handler runs Request.Validate first);
// tenantID comes from the verified principal, never the body.
//
// Before any of that, it gates on the tenant's billing entitlement: a tenant
// already at (or above) its active_process_limit is refused outright
// (ErrActivationBlocked, → 403), so it never upserts a row, never publishes the
// event, and never triggers the backfill listener — the ERD's edge-of-the-API
// entitlement gate, distinct from the worker's own per-record gate in sync.go
// (which lets a sync cycle continue past an individual blocked record).
func (uc *UseCase) ActivateIntegration(ctx context.Context, tenantID string, sources []string, scope Scope) ([]*Integration, error) {
	if err := uc.checkEntitlement(ctx, tenantID); err != nil {
		return nil, err
	}

	activated := make([]*Integration, 0, len(sources))

	err := uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		activated = activated[:0]
		for _, source := range sources {
			before, err := uc.currentIntegration(ctx, tx, tenantID, source)
			if err != nil {
				return err
			}

			after, err := uc.repo.Upsert(ctx, tx, tenantID, source, scope)
			if err != nil {
				return err
			}

			if activationChanged(before, after) {
				if err := uc.outbox.Publish(ctx, tx, newIntegrationActivated(after)); err != nil {
					return err
				}
			}

			activated = append(activated, after)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return activated, nil
}

// ListIntegrations returns the tenant's integrations for a read-only screen. It
// is a plain read model scoped to the tenant; no transaction or event.
func (uc *UseCase) ListIntegrations(ctx context.Context, tenantID string) ([]*Integration, error) {
	return uc.repo.List(ctx, tenantID)
}

// checkEntitlement refuses activation when the tenant's ACTIVE court record count
// already meets or exceeds its billing entitlement's active_process_limit. It
// reuses GetReconciliationTotals — the same pool read the reconciliations screen
// already runs to show the tenant's acquired acervo — rather than adding a second
// count query for the same number. Both reads (checker + repo) happen OUTSIDE any
// tx, same as sync.go's applyResult: a small overshoot under a concurrent
// activation is accepted (v0). A tenant with no subscription resolves to limit 0
// upstream (fail-closed), so it is blocked on its very first activation attempt.
func (uc *UseCase) checkEntitlement(ctx context.Context, tenantID string) error {
	limit, err := uc.checker.ActiveProcessLimit(ctx, tenantID)
	if err != nil {
		return err
	}
	totals, err := uc.repo.GetReconciliationTotals(ctx, tenantID)
	if err != nil {
		return err
	}
	if totals.CourtRecords >= limit {
		return ErrActivationBlocked
	}
	return nil
}

// currentIntegration returns the pre-upsert integration for (tenant, source), or
// nil when none exists yet (the first activation). A real lookup error
// propagates; only the typed not-found is folded into nil.
func (uc *UseCase) currentIntegration(ctx context.Context, tx database.Tx, tenantID, source string) (*Integration, error) {
	before, err := uc.repo.GetBySource(ctx, tx, tenantID, source)
	if errors.Is(err, ErrIntegrationNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return before, nil
}

// activationChanged reports whether an activation is meaningful enough to emit
// the event. A first activation (no prior row) always is; otherwise the event
// fires when the source was not already ACTIVE (a re-activation) or its scope
// changed. An identical, already-active re-post is not a change.
func activationChanged(before, after *Integration) bool {
	if before == nil {
		return true
	}
	return before.Status != StatusActive || !scopeEqual(before.Scope, after.Scope)
}

// scopeEqual compares two scopes by value. Order is significant: a reordered OAB
// list counts as a change, which is acceptable — the client controls the order
// and an identical re-post keeps it stable.
func scopeEqual(a, b Scope) bool {
	return slices.Equal(a.OAB, b.OAB) && slices.Equal(a.TaxID, b.TaxID)
}

// newIntegrationActivated builds the event for an activated integration, minting
// a fresh v7 event id (time-ordered) as the aggregate/idempotency key carrier.
func newIntegrationActivated(integ *Integration) IntegrationActivated {
	return IntegrationActivated{
		Base:          events.Base{EventID: uuid.Must(uuid.NewV7()).String(), Aggregate: integ.ID},
		IntegrationID: integ.ID,
		TenantID:      integ.TenantID,
		Source:        integ.Source,
		Scope:         integ.Scope,
	}
}

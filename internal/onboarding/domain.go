package onboarding

import (
	"context"

	"github.com/jusassessoria/platform/lib/database"
)

// UseCase orchestrates onboarding's two operations: read the activation
// progress, and dismiss the widget. It never imports another slice's entity
// or repository — every cross-table fact it needs is read through its own
// Repository (SQL in internal/onboarding/queries), never Go structs from
// acquisition/identity.
type UseCase struct {
	repo Repository
	uow  database.UnitOfWork
}

// NewUseCase wires the use case to its repository and unit of work.
func NewUseCase(repo Repository, uow database.UnitOfWork) *UseCase {
	return &UseCase{repo: repo, uow: uow}
}

// GetProgress reads the caller's tenant-wide activation Steps plus the
// caller's own dismissal timestamp — a plain pool read (no tx). tenantID and
// appUserID come from the verified principal, never the request, so a caller
// only ever sees its own tenant's progress and its own dismissal state.
func (uc *UseCase) GetProgress(ctx context.Context, tenantID, appUserID string) (Progress, error) {
	return uc.repo.GetProgress(ctx, tenantID, appUserID)
}

// Dismiss records that the caller dismissed the widget: upserts
// onboarding_widget_dismissal.dismissed_at = now() inside the caller's own
// tx. Idempotent — dismissing twice just restamps the same row, never an
// error. tenantID and appUserID come from the verified principal, never the
// request.
func (uc *UseCase) Dismiss(ctx context.Context, tenantID, appUserID string) error {
	return uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		return uc.repo.Dismiss(ctx, tx, tenantID, appUserID)
	})
}

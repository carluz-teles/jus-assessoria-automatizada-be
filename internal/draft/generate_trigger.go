package draft

import (
	"context"

	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// generate_trigger.go implements the synchronous trigger path: POST /v1/pecas/:id/generate.
// It opens a tenant-scoped tx, guards the saga state, flips it to EXTRACTING, and
// publishes draft.generation_requested in the same tx — then returns 202.
// The actual LLM generation happens asynchronously in worker-ai (generate.go).

// TriggerGenerationCommand is the input for POST /v1/pecas/:id/generate.
type TriggerGenerationCommand struct {
	TenantID string
	DraftID  string
}

// ErrGenerationInProgress is returned when the draft's saga_state is already
// EXTRACTING (generation is already running). Maps to 409 CONFLICT at the edge.
var ErrGenerationInProgress = errGenerationInProgress

// TriggerUseCase owns the synchronous trigger path. It depends on a narrow set of
// ports (the repository's reader/writer for the draft + the outbox) so it can be
// composed independently of the full UseCase.
type TriggerUseCase struct {
	uow    database.UnitOfWork
	rw     Repository
	outbox *events.Outbox
}

// NewTriggerUseCase wires the trigger use case.
func NewTriggerUseCase(uow database.UnitOfWork, rw Repository, outbox *events.Outbox) *TriggerUseCase {
	return &TriggerUseCase{uow: uow, rw: rw, outbox: outbox}
}

// TriggerGeneration implements POST /v1/pecas/:id/generate. In ONE tenant-scoped tx:
//  1. GetDraftByID (tenant guard → 404 if not found).
//  2. Guard saga_state ∈ {CREATED, REVIEWED, FAILED}; if EXTRACTING → 409.
//  3. UpdateSagaState → EXTRACTING (content unchanged).
//  4. outbox.Publish(draft.generation_requested) — same tx as the state flip.
//  5. Commit → returns the updated draft (saga_state=EXTRACTING).
//
// Regenerating from REVIEWED or FAILED is valid (same route, same logic).
func (uc *TriggerUseCase) TriggerGeneration(ctx context.Context, cmd TriggerGenerationCommand) (*Draft, error) {
	var updated *Draft

	err := uc.uow.Do(ctx, cmd.TenantID, func(tx database.Tx) error {
		d, err := uc.rw.GetDraftByID(ctx, tx, cmd.TenantID, cmd.DraftID)
		if err != nil {
			return err
		}

		if d.SagaState == SagaStateExtracting {
			return ErrGenerationInProgress
		}

		u, err := uc.rw.UpdateSagaState(ctx, tx, cmd.DraftID, cmd.TenantID, SagaStateExtracting, false, "")
		if err != nil {
			return err
		}
		updated = u

		ev := newGenerationRequested(updated)
		return uc.outbox.Publish(ctx, tx, ev)
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

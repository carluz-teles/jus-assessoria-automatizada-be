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

// TriggerGenerationCommand is the input for POST /v1/pecas/:id/generate. Tone,
// Instructions, and Theses are the Fatia 5 generation params — all optional
// (backward-compat with the pre-Fatia-5 empty body): Tone empty defaults to
// ToneTecnicoFormal server-side (identical prompt wording to before Fatia 5).
type TriggerGenerationCommand struct {
	TenantID     string
	DraftID      string
	Tone         string
	Instructions string
	Theses       []string
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
//  2. Guard: if EXTRACTING → 409 (generation already running).
//     Any other state (CREATED, DRAFTED, REVIEWED, FAILED) → allowed (regeneration is valid).
//  3. SetGenerationParams (tone/instructions/theses, Fatia 5) — persisted on the row
//     so the async worker (OnGenerationRequested) rereads them; the event payload
//     does not carry them.
//  4. UpdateSagaState → EXTRACTING (content unchanged).
//  5. outbox.Publish(draft.generation_requested) — same tx as the state flip.
//  6. Commit → returns the updated draft (saga_state=EXTRACTING).
//
// Regenerating from DRAFTED, REVIEWED, or FAILED is valid (same route, same logic).
func (uc *TriggerUseCase) TriggerGeneration(ctx context.Context, cmd TriggerGenerationCommand) (*Draft, error) {
	tone := cmd.Tone
	if tone == "" {
		tone = ToneTecnicoFormal
	}

	var updated *Draft

	err := uc.uow.Do(ctx, cmd.TenantID, func(tx database.Tx) error {
		d, err := uc.rw.GetDraftByID(ctx, tx, cmd.TenantID, cmd.DraftID)
		if err != nil {
			return err
		}

		// Only block if generation is already in flight — all other states allow re-triggering.
		if d.SagaState == SagaStateExtracting {
			return ErrGenerationInProgress
		}

		if err := uc.rw.SetGenerationParams(ctx, tx, cmd.DraftID, cmd.TenantID, tone, cmd.Instructions, cmd.Theses); err != nil {
			return err
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

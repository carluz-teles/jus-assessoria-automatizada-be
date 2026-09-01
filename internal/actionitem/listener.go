package actionitem

import (
	"context"
	"fmt"

	"github.com/hibiken/asynq"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/events"
)

// useCase is the port the listener delegates to.
type useCase interface {
	OnIntimationAnalyzed(ctx context.Context, ev IntimationAnalyzed) error
	OnTaskCreated(ctx context.Context, ev TaskCreated) error
}

// Listener is the actionitem slice's asynq consumer — its only async entry point
// (materialization from acquisition's analysis event). It holds no transport state; the
// use case owns persistence and the transaction boundary.
type Listener struct {
	uc useCase
}

// NewListener wires the listener to the use case.
func NewListener(uc useCase) *Listener {
	return &Listener{uc: uc}
}

// Register mounts the slice's task handlers on the asynq mux. acquisition.intimation.
// analyzed routes to the "ingestao" queue (lib/events' queueFor: every acquisition.*
// event does), so this mounts on the SAME mux as acquisition's own listeners — no
// dedicated server needed. task.created (fatia 3, the reverse half of the providência→
// tarefa loop) also routes to "ingestao" (lib/events' queueFor routes the "task" prefix
// there), so it rides the same mux too.
func (l *Listener) Register(mux *asynq.ServeMux) {
	mux.HandleFunc(TypeIntimationAnalyzed, l.handleIntimationAnalyzed)
	mux.HandleFunc(TypeTaskCreated, l.handleTaskCreated)
}

// handleIntimationAnalyzed is the asynq.HandlerFunc for acquisition.intimation.analyzed.
// It decodes the payload into the LOCAL shape and hands off to the use case, mapping the
// outcome to asynq's retry decision: a terminal domain error (KindInvalid/KindNotFound) is
// wrapped with SkipRetry so the task is archived; everything else (infra/unavailable) stays
// retryable. The ctx already carries the producer's trace and the consumer span
// (events.Observe middleware).
func (l *Listener) handleIntimationAnalyzed(ctx context.Context, t *asynq.Task) error {
	ev, err := events.Decode[IntimationAnalyzed](t)
	if err != nil {
		return err
	}
	if err := l.uc.OnIntimationAnalyzed(ctx, ev); err != nil {
		if isTerminal(err) {
			return fmt.Errorf("%w: %w", err, asynq.SkipRetry)
		}
		return err
	}
	return nil
}

// handleTaskCreated is the asynq.HandlerFunc for deadline.task.created (fatia 3). It decodes
// the LOCAL shape and hands off to the use case, which itself skips any event without an
// action_item_id (this handler never filters — that decision belongs to the domain, not the
// transport). Outcome mapping mirrors handleIntimationAnalyzed.
func (l *Listener) handleTaskCreated(ctx context.Context, t *asynq.Task) error {
	ev, err := events.Decode[TaskCreated](t)
	if err != nil {
		return err
	}
	if err := l.uc.OnTaskCreated(ctx, ev); err != nil {
		if isTerminal(err) {
			return fmt.Errorf("%w: %w", err, asynq.SkipRetry)
		}
		return err
	}
	return nil
}

// isTerminal reports whether a use-case error can never succeed on redelivery — an
// invalid materialized candidate (a bug in sanitizeCandidate/validate, never client input
// on this async path). Everything else stays retryable.
func isTerminal(err error) bool {
	ae, ok := apperr.From(err)
	if !ok {
		return false
	}
	return ae.Kind == apperr.KindInvalid
}

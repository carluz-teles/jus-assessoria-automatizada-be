package acquisition

import (
	"context"

	"github.com/hibiken/asynq"

	"github.com/jusassessoria/platform/lib/events"
)

// listener.go is the slice's async surface — the consumer counterpart of
// handler.go. It owns its task-type registration (Register), the worker only
// composes; and it is the molde for every future listener: extract the trace,
// decode the event with the shared codec, delegate to the use case, and let the
// error kind decide retry vs archive (a decode fault is SkipRetry, infra is
// retryable).

// listenerUC is the narrow port the listener drives — exactly the use case
// method behind the one task type this slice consumes.
type listenerUC interface {
	OnIntegrationActivated(ctx context.Context, ev IntegrationActivated) error
}

// Listener is acquisition's asynq consumer. It holds no transport state; the
// use case owns persistence and the transaction boundary.
type Listener struct {
	uc listenerUC
}

// NewListener wires the listener to the backfill use case.
func NewListener(uc listenerUC) *Listener {
	return &Listener{uc: uc}
}

// Register mounts the slice's task handlers on the asynq mux — the async analog
// of Handler.RegisterV1. Adding a consumed event = one HandleFunc here plus one
// Register call in the worker's composition root.
func (l *Listener) Register(mux *asynq.ServeMux) {
	mux.HandleFunc(TypeIntegrationActivated, l.handleIntegrationActivated)
}

// handleIntegrationActivated is the asynq.HandlerFunc for
// acquisition.integration_activated. It continues the producer's trace, decodes
// the payload, and hands off to the use case. A decode error is returned as-is
// (it wraps asynq.SkipRetry, so the task is archived, not retried); an infra
// error from the use case stays retryable.
func (l *Listener) handleIntegrationActivated(ctx context.Context, t *asynq.Task) error {
	ctx = events.ExtractTrace(ctx, t)
	ev, err := events.Decode[IntegrationActivated](t)
	if err != nil {
		return err
	}
	return l.uc.OnIntegrationActivated(ctx, ev)
}

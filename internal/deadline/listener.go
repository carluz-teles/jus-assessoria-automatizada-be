package deadline

import (
	"context"

	"github.com/hibiken/asynq"

	"github.com/jusassessoria/platform/lib/events"
)

// listener.go is the slice's async surface (there is no HTTP handler — this domain is
// driven entirely by events). It owns its task-type registration (Register); the worker
// only composes. Per task: decode with the shared codec (a decode fault is SkipRetry)
// and delegate to the use case. Trace continuation and the consumer span are handled
// once by the events.Observe middleware, not here.

// openUC is the port for the creation consumer (intimation.observed → deadline.opened).
type openUC interface {
	OnIntimationObserved(ctx context.Context, ev IntimationObserved) error
}

// Listener is the deadline slice's asynq consumer. It holds no transport state; the use
// case owns persistence and the transaction boundary.
type Listener struct {
	open openUC
}

// NewListener wires the listener to the creation use case.
func NewListener(open openUC) *Listener {
	return &Listener{open: open}
}

// Register mounts the slice's task handler on the asynq mux — the async analog of a
// Handler.Register. intimation.observed routes to the "ingestao" queue (acquisition
// prefix), so this is registered on the worker's main mux. Adding a consumed event =
// one HandleFunc here plus one Register call in the worker's composition root.
func (l *Listener) Register(mux *asynq.ServeMux) {
	mux.HandleFunc(TypeIntimationObserved, l.handleIntimationObserved)
}

// handleIntimationObserved is the asynq.HandlerFunc for acquisition.intimation.observed.
// It decodes the payload into the LOCAL shape and hands off to the use case. A decode
// error is returned as-is (it wraps asynq.SkipRetry, so a malformed task is archived,
// not retried); an infra error from the use case stays retryable. The ctx already
// carries the producer's trace and the consumer span (events.Observe middleware).
func (l *Listener) handleIntimationObserved(ctx context.Context, t *asynq.Task) error {
	ev, err := events.Decode[IntimationObserved](t)
	if err != nil {
		return err
	}
	return l.open.OnIntimationObserved(ctx, ev)
}

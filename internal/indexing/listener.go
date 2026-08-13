package indexing

import (
	"context"
	"fmt"

	"github.com/hibiken/asynq"

	"github.com/jusassessoria/platform/lib/events"
)

// listener.go is the fatia's async surface — it is driven entirely by events (no HTTP handler),
// mirroring internal/deadline's listener. It owns its task-type registration (Register); the
// worker only composes. Per task: decode with the shared codec (a decode fault is SkipRetry) and
// delegate to the use case, mapping a terminal domain error to asynq.SkipRetry. The consumer span
// + trace continuation are handled once by events.Observe middleware on the mux, not here.

// pipeline is the port the listener delegates to — the single async entry point (satisfied by
// *UseCase). Kept as an interface so the listener holds no concrete use-case state and tests can
// substitute a fake.
type pipeline interface {
	OnDocumentExtracted(ctx context.Context, ev DocumentExtracted) error
}

// Listener is the indexing slice's asynq consumer. It holds no transport state; the use case owns
// persistence and the transaction boundary.
type Listener struct {
	uc pipeline
}

// NewListener wires the listener to the indexing use case.
func NewListener(uc pipeline) *Listener {
	return &Listener{uc: uc}
}

// Register mounts the slice's task handler on the asynq mux — the async analog of a
// Handler.Register. document.extracted carries the "document" stream prefix; the worker composes
// this onto its mux (and the relay must route document.* to the queue that worker serves — see
// RegisterIndexingListeners's doc). Adding a consumed event = one HandleFunc here.
func (l *Listener) Register(mux *asynq.ServeMux) {
	mux.HandleFunc(TypeDocumentExtracted, l.handleDocumentExtracted)
}

// handleDocumentExtracted is the asynq.HandlerFunc for document.extracted. It decodes the payload
// into the LOCAL shape and hands off to the use case, then maps the outcome to asynq's retry
// decision: a decode error is returned as-is (it already wraps asynq.SkipRetry, so a malformed
// task is archived); a terminal domain error (malformed extracted-text, gone document) is wrapped
// with SkipRetry here (the transport owns the retry decision — the use case stays asynq-agnostic);
// every other error (infra: flaky storage/Voyage) stays retryable. The ctx already carries the
// producer's trace and the consumer span (events.Observe middleware).
func (l *Listener) handleDocumentExtracted(ctx context.Context, t *asynq.Task) error {
	ev, err := events.Decode[DocumentExtracted](t)
	if err != nil {
		return err
	}
	if err := l.uc.OnDocumentExtracted(ctx, ev); err != nil {
		if isTerminal(err) {
			return fmt.Errorf("%w: %w", err, asynq.SkipRetry)
		}
		return err
	}
	return nil
}

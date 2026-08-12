package deadline

import (
	"context"
	"fmt"

	"github.com/hibiken/asynq"

	"github.com/jusassessoria/platform/lib/apperr"
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
// It decodes the payload into the LOCAL shape and hands off to the use case, then maps
// the outcome to asynq's retry decision. A decode error is returned as-is (it already
// wraps asynq.SkipRetry, so a malformed task is archived, not retried); a terminal
// domain error is wrapped with SkipRetry here (the transport owns the retry decision —
// domain.go stays asynq-agnostic); every other error (infra/unavailable) stays
// retryable. The ctx already carries the producer's trace and the consumer span
// (events.Observe middleware).
func (l *Listener) handleIntimationObserved(ctx context.Context, t *asynq.Task) error {
	ev, err := events.Decode[IntimationObserved](t)
	if err != nil {
		return err
	}
	if err := l.open.OnIntimationObserved(ctx, ev); err != nil {
		if isTerminal(err) {
			return fmt.Errorf("%w: %w", err, asynq.SkipRetry)
		}
		return err
	}
	return nil
}

// isTerminal reports whether a use-case error can never succeed on redelivery, so the
// listener archives the task (asynq.SkipRetry) instead of burning the retry budget:
//   - KindInvalid — a malformed anchor (deadline_start_at) decoded from the event; the
//     payload is fixed, so a retry re-parses the same garbage.
//   - KindNotFound — a missing court_record or deadline_rule. The court_record is
//     committed upstream before intimation.observed is published (transactional outbox),
//     so its absence is a data/config fault, not a race the tenant's tx will heal; the
//     missing rule is a broken seed. Neither becomes present on retry.
//
// Everything else (KindInfra, KindUnavailable) stays retryable — asynq backs off and
// only archives to the DLQ once the attempt budget is spent. KindConflict never reaches
// here: the use case treats ErrDeadlineExists as an idempotent no-op (returns nil).
func isTerminal(err error) bool {
	ae, ok := apperr.From(err)
	if !ok {
		return false
	}
	return ae.Kind == apperr.KindInvalid || ae.Kind == apperr.KindNotFound
}

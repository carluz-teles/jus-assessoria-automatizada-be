package extraction

import (
	"context"
	"fmt"

	"github.com/hibiken/asynq"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/events"
)

// listener.go is the slice's async surface (there is no HTTP handler — this domain is
// driven entirely by document.uploaded). It owns its task-type registration; the worker
// only composes. It decodes with the shared codec (a decode fault is SkipRetry) and
// delegates to the use case, mapping the outcome to asynq's retry decision. Trace
// continuation and the consumer span are handled once by events.Observe, not here.

// useCase is the port the listener delegates to — the slice's single async entry point,
// satisfied by *UseCase. Kept as an interface so the listener test injects a stub.
type useCase interface {
	OnDocumentUploaded(ctx context.Context, ev DocumentUploaded) error
}

// Listener is the extraction slice's asynq consumer. It holds no transport state; the use
// case owns persistence and the transaction boundary.
type Listener struct {
	uc useCase
}

// NewListener wires the listener to the extraction use case.
func NewListener(uc useCase) *Listener {
	return &Listener{uc: uc}
}

// Register mounts the slice's handler on the asynq mux — the async analog of a
// Handler.Register. document.uploaded routes to the document worker's queue; the worker
// mounts this once.
func (l *Listener) Register(mux *asynq.ServeMux) {
	mux.HandleFunc(typeDocumentUploaded, l.handleDocumentUploaded)
}

// handleDocumentUploaded is the asynq.HandlerFunc for document.uploaded. It decodes the
// payload into the LOCAL shape and hands off to the use case, then maps the outcome to
// asynq's retry decision. A decode error is returned as-is (it already wraps
// asynq.SkipRetry, so a malformed task is archived, not retried); a terminal domain error
// is wrapped with SkipRetry here (the transport owns the retry decision); every other error
// (infra/unavailable — a bucket blip, the OCR API down) stays retryable. The ctx already
// carries the producer's trace and the consumer span (events.Observe middleware).
func (l *Listener) handleDocumentUploaded(ctx context.Context, t *asynq.Task) error {
	ev, err := events.Decode[DocumentUploaded](t)
	if err != nil {
		return err
	}
	if err := l.uc.OnDocumentUploaded(ctx, ev); err != nil {
		if isTerminal(err) {
			return fmt.Errorf("%w: %w", err, asynq.SkipRetry)
		}
		return err
	}
	return nil
}

// isTerminal reports whether a use-case error can never succeed on redelivery, so the
// listener archives the task (asynq.SkipRetry) instead of burning the retry budget:
//   - KindInvalid — a permanently-bad input (a corrupt/unparseable PDF the text-layer
//     reader and OCR both reject); a retry re-reads the same bytes.
//   - KindNotFound — the document row or its object is gone. The row is committed upstream
//     before document.uploaded is published (transactional outbox), so its absence is a
//     data fault, not a race a retry heals.
//
// Everything else (KindInfra, KindUnavailable — a storage blip, the Anthropic API
// unreachable/rate-limited) stays retryable; asynq backs off and only archives to the DLQ
// once the attempt budget is spent.
func isTerminal(err error) bool {
	ae, ok := apperr.From(err)
	if !ok {
		return false
	}
	return ae.Kind == apperr.KindInvalid || ae.Kind == apperr.KindNotFound
}

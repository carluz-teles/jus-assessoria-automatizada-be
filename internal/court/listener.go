package court

import (
	"context"
	"fmt"

	"github.com/hibiken/asynq"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/events"
)

// listener.go is this slice's async surface for FetchAutos: acquisition's
// court_record_observed is the ARRIVAL trigger, fetch_autos_requested is the
// self-re-enqueuing batch job, fetch_autos_item_requested is the individual-record
// retry. Per task: decode with the shared codec (a decode fault is SkipRetry) and
// delegate to the use case, then map the outcome to asynq's retry decision.

// useCase is the port the listener delegates to.
type useCase interface {
	OnCourtRecordObserved(ctx context.Context, ev courtRecordObserved) error
	OnFetchAutosRequested(ctx context.Context, ev fetchAutosRequested) error
	OnFetchAutosItemRequested(ctx context.Context, ev fetchAutosItemRequested) error
}

// Listener is the court slice's asynq consumer. It holds no transport state; the
// use case owns persistence and the transaction boundary.
type Listener struct {
	uc useCase
}

// NewListener wires the listener to the court use case.
func NewListener(uc useCase) *Listener {
	return &Listener{uc: uc}
}

// Register mounts the slice's task handlers on the asynq mux — the async analog of
// Handler.RegisterV1. All three route to the SAME dedicated court_fetch queue
// (cmd/worker-court): low concurrency by design (the real limit is how many
// distinct court_connection exist, not how many workers are spun up — see the
// worker's own doc).
func (l *Listener) Register(mux *asynq.ServeMux) {
	mux.HandleFunc(TypeCourtRecordObserved, l.handleCourtRecordObserved)
	mux.HandleFunc(typeFetchAutosRequested, l.handleFetchAutosRequested)
	mux.HandleFunc(typeFetchAutosItemRequested, l.handleFetchAutosItemRequested)
}

func (l *Listener) handleCourtRecordObserved(ctx context.Context, t *asynq.Task) error {
	ev, err := events.Decode[courtRecordObserved](t)
	if err != nil {
		return err
	}
	if err := l.uc.OnCourtRecordObserved(ctx, ev); err != nil {
		if isTerminal(err) {
			return fmt.Errorf("%w: %w", err, asynq.SkipRetry)
		}
		return err
	}
	return nil
}

func (l *Listener) handleFetchAutosRequested(ctx context.Context, t *asynq.Task) error {
	ev, err := events.Decode[fetchAutosRequested](t)
	if err != nil {
		return err
	}
	if err := l.uc.OnFetchAutosRequested(ctx, ev); err != nil {
		if isTerminal(err) {
			return fmt.Errorf("%w: %w", err, asynq.SkipRetry)
		}
		return err
	}
	return nil
}

func (l *Listener) handleFetchAutosItemRequested(ctx context.Context, t *asynq.Task) error {
	ev, err := events.Decode[fetchAutosItemRequested](t)
	if err != nil {
		return err
	}
	if err := l.uc.OnFetchAutosItemRequested(ctx, ev); err != nil {
		if isTerminal(err) {
			return fmt.Errorf("%w: %w", err, asynq.SkipRetry)
		}
		return err
	}
	return nil
}

// isTerminal reports a failure retrying will never fix: bad/missing input
// (KindInvalid) or a resolved-away aggregate (KindNotFound — e.g. the connection
// was deleted between publish and delivery). Everything else (KindInfra,
// KindUnavailable — a portal fault, a transient DB blip) stays retryable; asynq
// backs off and only archives once the relay's MaxRetry budget is spent. Mirrors
// internal/deadline's own isTerminal exactly (same classification, same reasoning).
func isTerminal(err error) bool {
	ae, ok := apperr.From(err)
	if !ok {
		return false
	}
	return ae.Kind == apperr.KindInvalid || ae.Kind == apperr.KindNotFound
}

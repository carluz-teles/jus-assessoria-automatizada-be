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

// useCase is the port the listener delegates to — the deadline slice's two async entry
// points, both satisfied by *UseCase: creation (intimation.observed → deadline.opened) and
// revocation (intimation.cancelled → deadline.revoked). Kept as ONE interface because a
// single use case object owns both transitions; the listener holds no transport state.
type useCase interface {
	OnIntimationObserved(ctx context.Context, ev IntimationObserved) error
	OnIntimationCancelled(ctx context.Context, ev IntimationCancelled) error
	OnReminderCheck(ctx context.Context, ev DeadlineReminderCheck) error
	OnMissedCheck(ctx context.Context, ev DeadlineMissedCheck) error
}

// Listener is the deadline slice's asynq consumer. It holds no transport state; the use
// case owns persistence and the transaction boundary.
type Listener struct {
	uc useCase
}

// NewListener wires the listener to the deadline use case.
func NewListener(uc useCase) *Listener {
	return &Listener{uc: uc}
}

// Register mounts the slice's task handlers on the asynq mux — the async analog of a
// Handler.Register. Both intimation.observed and intimation.cancelled route to the
// "ingestao" queue (acquisition prefix), so they mount on the worker's main mux. Adding a
// consumed event = one HandleFunc here plus one Register call in the worker's composition.
func (l *Listener) Register(mux *asynq.ServeMux) {
	mux.HandleFunc(TypeIntimationObserved, l.handleIntimationObserved)
	mux.HandleFunc(TypeIntimationCancelled, l.handleIntimationCancelled)
	mux.HandleFunc(TypeDeadlineReminderCheck, l.handleReminderCheck)
	mux.HandleFunc(TypeDeadlineMissedCheck, l.handleMissedCheck)
	// deadline.opened has no functional consumer (read models JOIN the deadline table),
	// but it IS produced per prazo (high-volume: a backfill mints thousands). Without a
	// handler it fails "handler not found", retries, and — routed to the deadline queue by
	// queueFor — would clog it. A no-op ACK drains it cheaply. Register a real consumer here
	// if a downstream ever needs to react to a prazo being opened.
	mux.HandleFunc(TypeDeadlineOpened, l.handleOpenedNoop)
}

// handleOpenedNoop acknowledges deadline.opened without work. See Register.
func (l *Listener) handleOpenedNoop(_ context.Context, _ *asynq.Task) error { return nil }

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
	if err := l.uc.OnIntimationObserved(ctx, ev); err != nil {
		if isTerminal(err) {
			return fmt.Errorf("%w: %w", err, asynq.SkipRetry)
		}
		return err
	}
	return nil
}

// handleIntimationCancelled is the asynq.HandlerFunc for acquisition.intimation.cancelled,
// the revocation counterpart of handleIntimationObserved: decode into the LOCAL shape and
// delegate, mapping the outcome to asynq's retry decision the same way. The use case
// treats "no prazo to revoke" (ErrDeadlineNotFound) as an idempotent no-op and returns
// nil, so a terminal error never surfaces from that path; any error that does reach here
// is classified by isTerminal (a malformed payload was already archived at decode).
func (l *Listener) handleIntimationCancelled(ctx context.Context, t *asynq.Task) error {
	ev, err := events.Decode[IntimationCancelled](t)
	if err != nil {
		return err
	}
	if err := l.uc.OnIntimationCancelled(ctx, ev); err != nil {
		if isTerminal(err) {
			return fmt.Errorf("%w: %w", err, asynq.SkipRetry)
		}
		return err
	}
	return nil
}

// handleReminderCheck is the asynq.HandlerFunc for the slice's own deadline.reminder_check:
// the D-N mark fired at its ETA. It decodes the LOCAL scheduled shape and delegates to the
// re-check use case, mapping the outcome to asynq's retry decision exactly as the
// intimation handlers do. A gone prazo (ErrDeadlineNotFound) is terminal (SkipRetry); an
// obsolete-status prazo is an in-use-case no-op that returns nil.
func (l *Listener) handleReminderCheck(ctx context.Context, t *asynq.Task) error {
	ev, err := events.Decode[DeadlineReminderCheck](t)
	if err != nil {
		return err
	}
	if err := l.uc.OnReminderCheck(ctx, ev); err != nil {
		if isTerminal(err) {
			return fmt.Errorf("%w: %w", err, asynq.SkipRetry)
		}
		return err
	}
	return nil
}

// handleMissedCheck is the asynq.HandlerFunc for the slice's own deadline.missed_check: the
// D+1 carência fired. It decodes the LOCAL scheduled shape and delegates to the auto-miss
// use case, mapping the outcome to asynq's retry decision the same way. Every non-OPEN /
// not-overdue case is an in-use-case no-op (returns nil), so only genuine infra faults stay
// retryable.
func (l *Listener) handleMissedCheck(ctx context.Context, t *asynq.Task) error {
	ev, err := events.Decode[DeadlineMissedCheck](t)
	if err != nil {
		return err
	}
	if err := l.uc.OnMissedCheck(ctx, ev); err != nil {
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

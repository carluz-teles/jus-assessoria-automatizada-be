package notifications

import (
	"context"

	"github.com/hibiken/asynq"

	"github.com/jusassessoria/platform/lib/events"
)

// listener.go is the slice's async surface (there is no HTTP handler — this domain
// is driven entirely by events). It owns its task-type registration (Register); the
// worker only composes. Per task: decode with the shared codec (a decode fault is
// SkipRetry) and delegate to the use case. Trace continuation and the consumer span
// are handled once by the events.Observe middleware, not here.

// notifyUC is the port for the email consumer (notification.requested).
type notifyUC interface {
	OnNotificationRequested(ctx context.Context, ev NotificationRequested) error
}

// inAppUC is the port for the in-app consumers: the two acquisition events (slice 1a) and
// the two deadline events (fatia 4c) this slice turns into IN_APP avisos.
type inAppUC interface {
	OnBackfillFinished(ctx context.Context, ev BackfillFinished) error
	OnDocketEntryObserved(ctx context.Context, ev DocketEntryObserved) error
	OnDeadlineDueSoon(ctx context.Context, ev DeadlineDueSoon) error
	OnDeadlineMissed(ctx context.Context, ev DeadlineMissed) error
}

// Listener is notifications' asynq consumer. It holds no transport state; the use
// cases own persistence and the transaction boundary. It drives the email use case
// (notification.requested) and the in-app use case (backfill_finished, docket_entry_observed).
type Listener struct {
	notify notifyUC
	inApp  inAppUC
}

// NewListener wires the listener to the notify (email) and in-app use cases.
func NewListener(notify notifyUC, inApp inAppUC) *Listener {
	return &Listener{notify: notify, inApp: inApp}
}

// Register mounts the slice's task handlers on the asynq mux — the async analog of a
// Handler.Register. Adding a consumed event = one HandleFunc here plus one Register
// call in the worker's composition root. The backfill_finished/docket_entry_observed
// handlers replace acquisition's drainUnconsumed placeholder (a pattern is registered
// exactly once across the shared mux, so the drain there is removed in lockstep).
func (l *Listener) Register(mux *asynq.ServeMux) {
	mux.HandleFunc(TypeNotificationRequested, l.handleNotificationRequested)
	mux.HandleFunc(TypeBackfillFinished, l.handleBackfillFinished)
	mux.HandleFunc(TypeDocketEntryObserved, l.handleDocketEntryObserved)
	mux.HandleFunc(TypeDeadlineDueSoon, l.handleDeadlineDueSoon)
	mux.HandleFunc(TypeDeadlineMissed, l.handleDeadlineMissed)
}

// handleNotificationRequested is the asynq.HandlerFunc for notification.requested. It
// decodes the payload and hands off to the use case. A decode error is returned as-is
// (it wraps asynq.SkipRetry, so the task is archived, not retried); an infra error from
// the use case stays retryable. The ctx already carries the producer's trace and the
// consumer span (events.Observe middleware).
func (l *Listener) handleNotificationRequested(ctx context.Context, t *asynq.Task) error {
	ev, err := events.Decode[NotificationRequested](t)
	if err != nil {
		return err
	}
	return l.notify.OnNotificationRequested(ctx, ev)
}

// handleBackfillFinished is the asynq.HandlerFunc for acquisition.backfill_finished. It
// decodes the payload and hands off to the in-app use case (→ an import_finished aviso).
// A decode error wraps asynq.SkipRetry (archived, not retried); an infra error from the
// use case stays retryable.
func (l *Listener) handleBackfillFinished(ctx context.Context, t *asynq.Task) error {
	ev, err := events.Decode[BackfillFinished](t)
	if err != nil {
		return err
	}
	return l.inApp.OnBackfillFinished(ctx, ev)
}

// handleDocketEntryObserved is the asynq.HandlerFunc for acquisition.docket_entry_observed.
// It decodes the payload and hands off to the in-app use case (→ a new_andamento aviso,
// suppressed during onboarding). Same error contract as the other handlers.
func (l *Listener) handleDocketEntryObserved(ctx context.Context, t *asynq.Task) error {
	ev, err := events.Decode[DocketEntryObserved](t)
	if err != nil {
		return err
	}
	return l.inApp.OnDocketEntryObserved(ctx, ev)
}

// handleDeadlineDueSoon is the asynq.HandlerFunc for deadline.due_soon (fatia 4c). It decodes
// the payload and hands off to the in-app use case (→ a deadline-due-soon aviso). A decode
// error wraps asynq.SkipRetry (archived, not retried); an infra error from the use case stays
// retryable.
func (l *Listener) handleDeadlineDueSoon(ctx context.Context, t *asynq.Task) error {
	ev, err := events.Decode[DeadlineDueSoon](t)
	if err != nil {
		return err
	}
	return l.inApp.OnDeadlineDueSoon(ctx, ev)
}

// handleDeadlineMissed is the asynq.HandlerFunc for deadline.missed (fatia 4c). It decodes the
// payload and hands off to the in-app use case (→ a "Prazo vencido" aviso). Same error contract
// as the other handlers.
func (l *Listener) handleDeadlineMissed(ctx context.Context, t *asynq.Task) error {
	ev, err := events.Decode[DeadlineMissed](t)
	if err != nil {
		return err
	}
	return l.inApp.OnDeadlineMissed(ctx, ev)
}

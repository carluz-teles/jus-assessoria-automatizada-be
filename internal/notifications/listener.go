package notifications

import (
	"context"

	"github.com/hibiken/asynq"

	"github.com/jusassessoria/platform/lib/events"
)

// listener.go is the slice's async surface (there is no HTTP handler — this domain
// is driven entirely by events). It owns its task-type registration (Register); the
// worker only composes. Per task: continue the producer's trace, decode with the
// shared codec (a decode fault is SkipRetry), and delegate to the use case.

// notifyUC is the port the listener drives — the use case's single entry point.
type notifyUC interface {
	OnNotificationRequested(ctx context.Context, ev NotificationRequested) error
}

// Listener is notifications' asynq consumer. It holds no transport state; the use
// case owns persistence and the transaction boundary.
type Listener struct {
	uc notifyUC
}

// NewListener wires the listener to the notify use case.
func NewListener(uc notifyUC) *Listener { return &Listener{uc: uc} }

// Register mounts the slice's task handler on the asynq mux — the async analog of a
// Handler.Register. Adding a consumed event = one HandleFunc here plus one Register
// call in the worker's composition root.
func (l *Listener) Register(mux *asynq.ServeMux) {
	mux.HandleFunc(TypeNotificationRequested, l.handleNotificationRequested)
}

// handleNotificationRequested is the asynq.HandlerFunc for notification.requested. It
// continues the producer's trace, decodes the payload, and hands off to the use case.
// A decode error is returned as-is (it wraps asynq.SkipRetry, so the task is archived,
// not retried); an infra error from the use case stays retryable.
func (l *Listener) handleNotificationRequested(ctx context.Context, t *asynq.Task) error {
	ctx = events.ExtractTrace(ctx, t)
	ev, err := events.Decode[NotificationRequested](t)
	if err != nil {
		return err
	}
	return l.uc.OnNotificationRequested(ctx, ev)
}

package acquisition

import (
	"context"

	"github.com/hibiken/asynq"

	"github.com/jusassessoria/platform/lib/events"
)

// listener.go is the slice's async surface — the consumer counterpart of
// handler.go. It owns its task-type registration (Register); the worker only
// composes. The pattern per task is uniform: extract the producer's trace,
// decode the event with the shared codec, delegate to the use case, and let the
// error kind decide retry vs. archive (a decode fault is SkipRetry, infra is
// retryable). One Listener consumes every event this slice reacts to.

// backfillListenerUC is the port for the integration_activated consumer.
type backfillListenerUC interface {
	OnIntegrationActivated(ctx context.Context, ev IntegrationActivated) error
}

// syncListenerUC is the port for the sync_requested consumer.
type syncListenerUC interface {
	OnSyncRequested(ctx context.Context, ev SyncRequested) error
}

// Listener is acquisition's asynq consumer. It holds no transport state; the use
// cases own persistence and the transaction boundary. It drives two use cases —
// backfill (reacts to integration_activated) and sync (reacts to sync_requested).
type Listener struct {
	backfill backfillListenerUC
	sync     syncListenerUC
}

// NewListener wires the listener to the backfill and sync use cases.
func NewListener(backfill backfillListenerUC, sync syncListenerUC) *Listener {
	return &Listener{backfill: backfill, sync: sync}
}

// Register mounts the slice's task handlers on the asynq mux — the async analog
// of Handler.RegisterV1. Adding a consumed event = one HandleFunc here plus one
// Register call in the worker's composition root.
func (l *Listener) Register(mux *asynq.ServeMux) {
	mux.HandleFunc(TypeIntegrationActivated, l.handleIntegrationActivated)
	mux.HandleFunc(TypeSyncRequested, l.handleSyncRequested)
}

// handleIntegrationActivated is the asynq.HandlerFunc for
// acquisition.integration_activated. It continues the producer's trace, decodes
// the payload, and hands off to the backfill use case. A decode error is
// returned as-is (it wraps asynq.SkipRetry, so the task is archived, not
// retried); an infra error from the use case stays retryable.
func (l *Listener) handleIntegrationActivated(ctx context.Context, t *asynq.Task) error {
	ctx = events.ExtractTrace(ctx, t)
	ev, err := events.Decode[IntegrationActivated](t)
	if err != nil {
		return err
	}
	return l.backfill.OnIntegrationActivated(ctx, ev)
}

// handleSyncRequested is the asynq.HandlerFunc for acquisition.sync_requested. It
// continues the trace, decodes the payload, and hands off to the sync use case. A
// decode fault is SkipRetry; the use case itself returns SkipRetry on a parse
// fault and nil (ack) on a fetch fault (see OnSyncRequested).
func (l *Listener) handleSyncRequested(ctx context.Context, t *asynq.Task) error {
	ctx = events.ExtractTrace(ctx, t)
	ev, err := events.Decode[SyncRequested](t)
	if err != nil {
		return err
	}
	return l.sync.OnSyncRequested(ctx, ev)
}

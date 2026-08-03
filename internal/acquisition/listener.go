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

// backfillListenerUC is the port for the backfill consumers: onboarding
// (integration_activated) and the completion counter (sync_completed/failed).
type backfillListenerUC interface {
	OnIntegrationActivated(ctx context.Context, ev IntegrationActivated) error
	OnSyncCompleted(ctx context.Context, ev SyncCompleted) error
	OnSyncFailed(ctx context.Context, ev SyncFailed) error
}

// syncListenerUC is the port for the sync_requested consumer.
type syncListenerUC interface {
	OnSyncRequested(ctx context.Context, ev SyncRequested) error
}

// enrichmentListenerUC is the port for the court_record_observed consumer (DATAJUD
// enrichment).
type enrichmentListenerUC interface {
	OnCourtRecordObserved(ctx context.Context, ev CourtRecordObserved) error
}

// Listener is acquisition's asynq consumer. It holds no transport state; the use
// cases own persistence and the transaction boundary. It drives three use cases —
// backfill (reacts to integration_activated), sync (reacts to sync_requested), and
// enrichment (reacts to court_record_observed).
type Listener struct {
	backfill   backfillListenerUC
	sync       syncListenerUC
	enrichment enrichmentListenerUC
}

// NewListener wires the listener to the backfill, sync, and enrichment use cases.
func NewListener(backfill backfillListenerUC, sync syncListenerUC, enrichment enrichmentListenerUC) *Listener {
	return &Listener{backfill: backfill, sync: sync, enrichment: enrichment}
}

// Register mounts the slice's task handlers on the asynq mux — the async analog
// of Handler.RegisterV1. Adding a consumed event = one HandleFunc here plus one
// Register call in the worker's composition root.
func (l *Listener) Register(mux *asynq.ServeMux) {
	mux.HandleFunc(TypeIntegrationActivated, l.handleIntegrationActivated)
	mux.HandleFunc(TypeSyncRequested, l.handleSyncRequested)
	mux.HandleFunc(TypeSyncCompleted, l.handleSyncCompleted)
	mux.HandleFunc(TypeSyncFailed, l.handleSyncFailed)
	mux.HandleFunc(TypeCourtRecordObserved, l.handleCourtRecordObserved)
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

// handleSyncCompleted is the asynq.HandlerFunc for acquisition.sync_completed. It
// continues the trace, decodes the payload, and hands off to the backfill
// completion counter. A decode fault wraps asynq.SkipRetry (archived, not
// retried); an infra error from the use case stays retryable.
func (l *Listener) handleSyncCompleted(ctx context.Context, t *asynq.Task) error {
	ctx = events.ExtractTrace(ctx, t)
	ev, err := events.Decode[SyncCompleted](t)
	if err != nil {
		return err
	}
	return l.backfill.OnSyncCompleted(ctx, ev)
}

// handleSyncFailed is the asynq.HandlerFunc for acquisition.sync_failed — the
// failure-side counterpart of handleSyncCompleted, dispatching to the same
// completion counter so a failed slice still advances the job toward finish.
func (l *Listener) handleSyncFailed(ctx context.Context, t *asynq.Task) error {
	ctx = events.ExtractTrace(ctx, t)
	ev, err := events.Decode[SyncFailed](t)
	if err != nil {
		return err
	}
	return l.backfill.OnSyncFailed(ctx, ev)
}

// handleCourtRecordObserved is the asynq.HandlerFunc for
// acquisition.court_record_observed. It continues the trace, decodes the payload,
// and hands off to the DATAJUD enrichment use case. A decode fault is SkipRetry;
// the use case returns SkipRetry on a parse fault, a retryable error on a fetch
// fault, and nil (ack) when there is nothing to enrich.
func (l *Listener) handleCourtRecordObserved(ctx context.Context, t *asynq.Task) error {
	ctx = events.ExtractTrace(ctx, t)
	ev, err := events.Decode[CourtRecordObserved](t)
	if err != nil {
		return err
	}
	return l.enrichment.OnCourtRecordObserved(ctx, ev)
}

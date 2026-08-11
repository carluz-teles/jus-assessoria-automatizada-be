package acquisition

import (
	"context"

	"github.com/hibiken/asynq"

	"github.com/jusassessoria/platform/lib/events"
)

// listener.go is the slice's async surface — the consumer counterpart of
// handler.go. It owns its task-type registration (Register); the worker only
// composes. The pattern per task is uniform: decode the event with the shared
// codec, delegate to the use case, and let the error kind decide retry vs.
// archive (a decode fault is SkipRetry, infra is retryable). Trace continuation
// and the consumer span are handled once by the events.Observe middleware, so a
// handler receives a ctx that already carries them. One Listener consumes every
// event this slice reacts to.

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

// ingestionListenerUC is the port for the diario_requested consumer (national bulk
// ingestion). It is OPTIONAL: nil when INGESTION_ENABLED is off, in which case the
// diario_requested handler is not registered (the pivot ships inert).
type ingestionListenerUC interface {
	OnDiarioRequested(ctx context.Context, ev DiarioRequested) error
}

// Listener is acquisition's asynq consumer. It holds no transport state; the use
// cases own persistence and the transaction boundary. It drives up to four use cases —
// backfill (reacts to integration_activated), sync (reacts to sync_requested),
// enrichment (reacts to court_record_observed), and — when the national pivot is on —
// ingestion (reacts to diario_requested).
type Listener struct {
	backfill   backfillListenerUC
	sync       syncListenerUC
	enrichment enrichmentListenerUC
	ingestion  ingestionListenerUC
}

// NewListener wires the listener to the backfill, sync, and enrichment use cases, plus
// the optional ingestion consumer (nil disables the diario_requested handler).
func NewListener(backfill backfillListenerUC, sync syncListenerUC, enrichment enrichmentListenerUC, ingestion ingestionListenerUC) *Listener {
	return &Listener{backfill: backfill, sync: sync, enrichment: enrichment, ingestion: ingestion}
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

	// diario_requested (ingestão nacional) só tem consumer quando o pivot está ligado
	// (INGESTION_ENABLED injeta a use case). Registrar condicionalmente mantém o worker
	// inerte ao evento quando desligado — um pattern é registrado uma única vez no mux, e
	// registrar com uma use case nil daria panic de nil no dispatch.
	if l.ingestion != nil {
		mux.HandleFunc(TypeDiarioRequested, l.handleDiarioRequested)
	}

	// docket_entry_observed (novo andamento) e backfill_finished (aviso de fim) agora
	// têm consumer real: o slice notifications os transforma em avisos IN_APP e
	// registra os handlers no MESMO mux (worker-ingestao). Um pattern é registrado uma
	// única vez no mux, então este slice NÃO os registra — o antigo drainUnconsumed foi
	// removido junto com o registro em notifications (senão: só-remover=flood volta,
	// só-adicionar=panic de pattern duplicado no boot).
}

// handleIntegrationActivated is the asynq.HandlerFunc for
// acquisition.integration_activated. It decodes the payload, and hands off to
// the backfill use case. A decode error is
// returned as-is (it wraps asynq.SkipRetry, so the task is archived, not
// retried); an infra error from the use case stays retryable.
func (l *Listener) handleIntegrationActivated(ctx context.Context, t *asynq.Task) error {
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
	ev, err := events.Decode[SyncFailed](t)
	if err != nil {
		return err
	}
	return l.backfill.OnSyncFailed(ctx, ev)
}

// handleCourtRecordObserved is the asynq.HandlerFunc for
// acquisition.court_record_observed. It decodes the payload,
// and hands off to the DATAJUD enrichment use case. A decode fault is SkipRetry;
// the use case returns SkipRetry on a parse fault, a retryable error on a fetch
// fault, and nil (ack) when there is nothing to enrich.
func (l *Listener) handleCourtRecordObserved(ctx context.Context, t *asynq.Task) error {
	ev, err := events.Decode[CourtRecordObserved](t)
	if err != nil {
		return err
	}
	return l.enrichment.OnCourtRecordObserved(ctx, ev)
}

// handleDiarioRequested is the asynq.HandlerFunc for acquisition.diario_requested. It
// decodes the payload and hands off to the ingestion use case. A decode fault is
// SkipRetry (archived, never parses on retry); the use case returns a retryable error on
// a fetch/DB fault (asynq retries with backoff, then archives to the DLQ) and SkipRetry
// on a parse fault.
func (l *Listener) handleDiarioRequested(ctx context.Context, t *asynq.Task) error {
	ev, err := events.Decode[DiarioRequested](t)
	if err != nil {
		return err
	}
	return l.ingestion.OnDiarioRequested(ctx, ev)
}

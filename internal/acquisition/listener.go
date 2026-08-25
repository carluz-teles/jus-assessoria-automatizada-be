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
	// OnWatchedOABReenabled reacts to the Termos re-enable toggle (ToggleWatchedOAB):
	// it fires a catch-up scoped to just the downtime of the ONE OAB that was
	// switched back on — see backfill.go.
	OnWatchedOABReenabled(ctx context.Context, ev WatchedOABReenabled) error
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

// enrichmentBatchListenerUC is the port for the enrichment_batch_requested consumer: the
// self-re-enqueuing batch job that enriches one (tenant, tribunal) of an import in LOTE and
// owns the import's ENRICHMENT capture row lifecycle (opens, progresses, closes).
type enrichmentBatchListenerUC interface {
	OnEnrichmentBatchRequested(ctx context.Context, ev EnrichmentBatchRequested) error
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
	backfill        backfillListenerUC
	sync            syncListenerUC
	enrichment      enrichmentListenerUC
	enrichmentBatch enrichmentBatchListenerUC
	ingestion       ingestionListenerUC
}

// NewListener wires the listener to the backfill, sync, enrichment, and enrichment-batch
// use cases, plus the optional ingestion consumer (nil disables the diario_requested handler).
func NewListener(backfill backfillListenerUC, sync syncListenerUC, enrichment enrichmentListenerUC, enrichmentBatch enrichmentBatchListenerUC, ingestion ingestionListenerUC) *Listener {
	return &Listener{
		backfill:        backfill,
		sync:            sync,
		enrichment:      enrichment,
		enrichmentBatch: enrichmentBatch,
		ingestion:       ingestion,
	}
}

// Register mounts the slice's task handlers on the asynq mux — the async analog
// of Handler.RegisterV1. Adding a consumed event = one HandleFunc here plus one
// Register call in the worker's composition root.
func (l *Listener) Register(mux *asynq.ServeMux) {
	mux.HandleFunc(TypeIntegrationActivated, l.handleIntegrationActivated)
	mux.HandleFunc(TypeWatchedOABReenabled, l.handleWatchedOABReenabled)
	mux.HandleFunc(TypeSyncRequested, l.handleSyncRequested)
	mux.HandleFunc(TypeCourtRecordObserved, l.handleCourtRecordObserved)

	// enrichment_batch_requested is NOT registered here: it lives on its OWN "enrichment"
	// queue, mounted on a dedicated low-concurrency server via RegisterEnrichment, so the
	// batch DATAJUD job (serialized by its own rate limiter) is isolated from the sync work
	// on "ingestao". A pattern may be registered only once, so it lives there and NOT here.

	// sync_completed/sync_failed are NOT registered here: they live on their OWN
	// "sync_status" queue and are mounted on a dedicated concurrency-2 server via
	// RegisterSyncStatus, so the light backfill completion counter finalizes the job
	// without waiting behind this queue's slow enrichment flood. A pattern may be
	// registered only ONCE — on the mux of the server that serves its queue — so keeping
	// them here would either shadow the dedicated server or panic on a duplicate pattern.

	// diario_requested is NOT registered here: it lives on its OWN queue and is mounted
	// on a dedicated concurrency-1 server via RegisterIngestion, so the national bulk
	// fetch is serialized and isolated from this queue's enrichment/sync work.

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

// handleWatchedOABReenabled is the asynq.HandlerFunc for
// acquisition.watched_oab_reenabled. It decodes the payload and hands off to the
// backfill use case's catch-up path. A decode fault is SkipRetry; an infra error
// from the use case stays retryable.
func (l *Listener) handleWatchedOABReenabled(ctx context.Context, t *asynq.Task) error {
	ev, err := events.Decode[WatchedOABReenabled](t)
	if err != nil {
		return err
	}
	return l.backfill.OnWatchedOABReenabled(ctx, ev)
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

// RegisterEnrichment mounts the enrichment_batch_requested handler on a mux — meant for the
// worker's DEDICATED low-concurrency "enrichment" server, so the batch DATAJUD job (already
// serialized by its own rate limiter) never competes with the sync work on "ingestao". A
// pattern may be registered only once, so this handler lives here and NOT in Register.
func (l *Listener) RegisterEnrichment(mux *asynq.ServeMux) {
	mux.HandleFunc(TypeEnrichmentBatchRequested, l.handleEnrichmentBatchRequested)
}

// handleEnrichmentBatchRequested is the asynq.HandlerFunc for
// acquisition.enrichment_batch_requested — one step of the batch enrichment job. It decodes
// the payload and hands off to the batch use case. A decode fault is SkipRetry; the use case
// returns a retryable error on a fetch fault, SkipRetry on a parse fault, and nil (ack) when
// there is nothing left to enrich (it closed the import).
func (l *Listener) handleEnrichmentBatchRequested(ctx context.Context, t *asynq.Task) error {
	ev, err := events.Decode[EnrichmentBatchRequested](t)
	if err != nil {
		return err
	}
	return l.enrichmentBatch.OnEnrichmentBatchRequested(ctx, ev)
}

// RegisterSyncStatus mounts the sync_completed/sync_failed handlers on a mux — meant
// for the worker's DEDICATED concurrency-2 server (its own "sync_status" queue), so the
// light backfill completion counter finalizes the job independently of the slow DATAJUD
// enrichment on the "ingestao" queue. A pattern may be registered only once, so these two
// handlers live here and NOT in Register (see Register's comment for the rationale).
func (l *Listener) RegisterSyncStatus(mux *asynq.ServeMux) {
	mux.HandleFunc(TypeSyncCompleted, l.handleSyncCompleted)
	mux.HandleFunc(TypeSyncFailed, l.handleSyncFailed)
}

// RegisterIngestion mounts the diario_requested handler on a mux — meant for the
// worker's DEDICATED concurrency-1 server (its own "diario" queue), so the national
// bulk fetch runs serialized. Called only when the ingestion use case is wired
// (INGESTION_ENABLED); the caller guards the nil so no nil handler is ever registered.
func (l *Listener) RegisterIngestion(mux *asynq.ServeMux) {
	mux.HandleFunc(TypeDiarioRequested, l.handleDiarioRequested)
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

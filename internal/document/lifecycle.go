package document

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/hibiken/asynq"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/jusassessoria/platform/lib/events"
	"github.com/jusassessoria/platform/lib/obs"
)

// lifecycle.go is the Documentos pipeline's TERMINAL observer — the consumer nobody had. The
// extraction + indexing slices PRODUCE document.ready / document.failed (the notification
// facts a UI/operator watches), but nothing consumed them: the relay routes document.* to the
// "documents" queue, asynq found no handler, logged `handler not found for task
// "document.failed"` and retried the notice up to 10× (spam + waste). These are terminal
// notifications, not work — so this observer mounts handlers that ACK them (return nil, no
// retry) and, since it already sits on the pipeline's exit, records the completion telemetry
// (the ready/failed counters + the chunk-count distribution) there.

// Consumed task types this observer handles. They mirror the ids the extraction/indexing
// slices produce (document.ready is indexing's success fact; document.failed is either slice's
// failure fact). Kept as local literals — the observer decodes both into local shapes and never
// imports the producing slices (no cross-slice import).
const (
	typeDocumentReady  = "document.ready"
	typeDocumentFailed = "document.failed"
)

// Metric names for the pipeline's terminal outcome. Dotted + prefixed like asynq.queue.depth so
// the backend groups the whole document.* family together.
const (
	metricPipelineCompleted = "document.pipeline.completed"
	metricPipelineChunks    = "document.pipeline.chunks"
)

// documentReady is the LOCAL decode shape of document.ready (indexing's success fact): the
// document + tenant, the chunk count indexed and the embedding model that produced the vectors.
// Decode-only (never published from here), so it carries no Event methods.
type documentReady struct {
	events.Base
	DocumentID     string `json:"document_id"`
	TenantID       string `json:"tenant_id"`
	ChunkCount     int    `json:"chunk_count"`
	EmbeddingModel string `json:"embedding_model"`
}

// documentFailed is the LOCAL decode shape of document.failed (either pipeline slice's failure
// fact): the document + tenant, the stage that faulted (extraction|indexing) and the error
// message. Decode-only.
type documentFailed struct {
	events.Base
	DocumentID string `json:"document_id"`
	TenantID   string `json:"tenant_id"`
	Stage      string `json:"stage"`
	Error      string `json:"error"`
}

// LifecycleObserver consumes the Documentos pipeline's terminal notifications. It owns no
// persistence and no transaction — it observes: it logs the outcome, records the completion
// metrics, enriches the active consumer span, and ACKs (returns nil) so a terminal notice is
// never retried.
type LifecycleObserver struct {
	logger    *slog.Logger
	completed metric.Int64Counter
	chunks    metric.Int64Histogram
}

// NewLifecycleObserver builds the observer and its metric instruments once (via obs.Meter(),
// mirroring lib/events/queue_metrics.go). Instrument creation can fail; the error is surfaced so
// the worker fails boot loudly (fail-fast) rather than silently dropping telemetry. logger is
// the worker's structured logger.
func NewLifecycleObserver(logger *slog.Logger) (*LifecycleObserver, error) {
	meter := obs.Meter()

	completed, err := meter.Int64Counter(
		metricPipelineCompleted,
		metric.WithDescription("Documents whose pipeline reached a terminal state, by outcome (ready|failed) and stage."),
	)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: create %s counter: %w", metricPipelineCompleted, err)
	}

	chunks, err := meter.Int64Histogram(
		metricPipelineChunks,
		metric.WithDescription("Chunks indexed per document when it reaches READY."),
	)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: create %s histogram: %w", metricPipelineChunks, err)
	}

	return &LifecycleObserver{logger: logger, completed: completed, chunks: chunks}, nil
}

// Register mounts the terminal handlers on the asynq mux — the fix for the missing consumer. The
// worker calls it once (inside the S3Enabled block, next to the extraction/indexing
// registration). Both ids route to the "documents" queue the worker serves.
func (o *LifecycleObserver) Register(mux *asynq.ServeMux) {
	mux.HandleFunc(typeDocumentReady, o.handleReady)
	mux.HandleFunc(typeDocumentFailed, o.handleFailed)
}

// handleReady is the asynq.HandlerFunc for document.ready. It logs the completion, increments the
// completed counter (outcome=ready), records the chunk count on the distribution, and enriches
// the active consumer span (events.Observe already opened it). It ALWAYS returns nil: a ready
// notice is terminal, so a decode fault or a nil instrument must not re-spam the queue.
func (o *LifecycleObserver) handleReady(ctx context.Context, t *asynq.Task) error {
	ev, err := events.Decode[documentReady](t)
	if err != nil {
		// A malformed terminal notice can never parse on retry — ack it (return nil) so it
		// does not re-spam. Observe already logged the decode via the span; note it here too.
		o.logger.LogAttrs(ctx, slog.LevelError, "document ready: decode failed",
			slog.String("error", err.Error()))
		return nil
	}

	o.logger.LogAttrs(ctx, slog.LevelInfo, "document ready",
		slog.String("document_id", ev.DocumentID),
		slog.String("tenant_id", ev.TenantID),
		slog.Int("chunk_count", ev.ChunkCount),
		slog.String("embedding_model", ev.EmbeddingModel),
	)

	o.completed.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", "ready")))
	o.chunks.Record(ctx, int64(ev.ChunkCount))

	span := trace.SpanFromContext(ctx)
	span.SetAttributes(
		attribute.String("document.id", ev.DocumentID),
		attribute.Int("document.chunk_count", ev.ChunkCount),
	)
	return nil
}

// handleFailed is the asynq.HandlerFunc for document.failed. It logs the failure (the terminal
// notice's own error), increments the completed counter (outcome=failed, faceted by stage), and
// enriches the span. It ALWAYS returns nil — a failure NOTICE must never be retried, or the
// pipeline re-spams the very failure it reported (the original bug).
func (o *LifecycleObserver) handleFailed(ctx context.Context, t *asynq.Task) error {
	ev, err := events.Decode[documentFailed](t)
	if err != nil {
		o.logger.LogAttrs(ctx, slog.LevelError, "document failed: decode failed",
			slog.String("error", err.Error()))
		return nil
	}

	o.logger.LogAttrs(ctx, slog.LevelError, "document pipeline failed",
		slog.String("document_id", ev.DocumentID),
		slog.String("tenant_id", ev.TenantID),
		slog.String("stage", ev.Stage),
		slog.String("error", ev.Error),
	)

	o.completed.Add(ctx, 1, metric.WithAttributes(
		attribute.String("outcome", "failed"),
		attribute.String("stage", ev.Stage),
	))

	span := trace.SpanFromContext(ctx)
	span.SetAttributes(
		attribute.String("document.id", ev.DocumentID),
		attribute.String("document.stage", ev.Stage),
	)
	return nil
}

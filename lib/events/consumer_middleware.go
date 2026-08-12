package events

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/hibiken/asynq"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/jusassessoria/platform/lib/obs"
)

// Task headers the relay stamps besides traceparent, so the consumer span and
// failure log carry the event's identity WITHOUT decoding the payload (the
// middleware is domain-agnostic). See Relay.publish.
const (
	eventIDHeader     = "event-id"
	aggregateIDHeader = "aggregate-id"
)

// Messaging semantic-convention keys for the consumer span. Kept as literals so a
// semconv version bump never silently renames the span attributes the backend
// dashboards facet on.
const (
	attrMessagingSystem = "messaging.system"
	attrMessagingDest   = "messaging.destination.name"
	attrMessagingOp     = "messaging.operation"
	attrMessagingMsgID  = "messaging.message.id"

	taskIDKey = "task_id"
)

// Observe instruments EVERY consumed event the same way — the async-side mirror
// of the HTTP otelfiber+RequestLogger pair. Wire it once per worker with
// mux.Use(events.Observe(logger)); every slice's listener is covered without a
// line of per-handler code.
//
// For each task it:
//  1. reads the producer's span context from the task header (this replaces the
//     per-handler events.ExtractTrace calls — extraction now happens here, once);
//  2. opens a CONSUMER span "<event.type> process" as a NEW ROOT LINKED to the
//     producer, so each consumed event is its own bounded, navigable trace instead of
//     one giant tree — a big backfill fans into thousands of small linked traces, not a
//     single 40k-span monster. The link preserves causality; the sampler judges each
//     unit independently (lib/telemetry importTraceSampler). Correlate a whole run by
//     the business ids on the spans (tenant_id, integration_id, …), not by trace walls;
//  3. sets the span status from the handler outcome (Ok, or Error+exception);
//  4. logs ONLY on failure — the happy path is told by the span (spans for flow,
//     logs for failures, to keep New Relic ingest lean).
//
// The failure line carries the keys needed to FIND it: event_type, queue, retry,
// outcome (retryable vs dropped), event_id, aggregate_id and duration; trace_id
// and span_id are stamped automatically by lib/telemetry because the span is
// active. Returning the error is left intact so asynq keeps its retry/archive
// semantics — this is the single async error boundary, like WriteError is for HTTP.
func Observe(logger *slog.Logger) asynq.MiddlewareFunc {
	return func(next asynq.Handler) asynq.Handler {
		return asynq.HandlerFunc(func(ctx context.Context, t *asynq.Task) error {
			start := time.Now()

			// Read the producer's span context for the LINK, then open the consumer span
			// as a new root linking back to it — each event is its own trace (see doc).
			producer := trace.SpanContextFromContext(ExtractTrace(ctx, t))
			queue, _ := asynq.GetQueueName(ctx)
			retry, _ := asynq.GetRetryCount(ctx)
			maxRetry, _ := asynq.GetMaxRetry(ctx)
			taskID, _ := asynq.GetTaskID(ctx)
			eventID := t.Headers()[eventIDHeader]
			aggregateID := t.Headers()[aggregateIDHeader]

			startOpts := []trace.SpanStartOption{
				trace.WithSpanKind(trace.SpanKindConsumer),
				trace.WithNewRoot(),
				trace.WithAttributes(
					attribute.String(attrMessagingSystem, "asynq"),
					attribute.String(attrMessagingDest, queue),
					attribute.String(attrMessagingOp, "process"),
					attribute.String(attrMessagingMsgID, eventID),
					attribute.String(obs.KeyEventType, t.Type()),
					attribute.String(obs.KeyAggregateID, aggregateID),
					attribute.Int(obs.KeyRetry, retry),
				),
			}
			if producer.IsValid() {
				startOpts = append(startOpts, trace.WithLinks(trace.Link{SpanContext: producer}))
			}

			ctx, span := obs.Start(ctx, t.Type()+" process", startOpts...)
			defer span.End()

			err := next.ProcessTask(ctx, t)
			obs.Record(span, err)
			if err != nil {
				logger.LogAttrs(ctx, slog.LevelError, "event failed",
					slog.String(obs.KeyEventType, t.Type()),
					slog.String(obs.KeyQueue, queue),
					slog.Int(obs.KeyRetry, retry),
					slog.Int(obs.KeyMaxRetry, maxRetry),
					slog.String(obs.KeyEventID, eventID),
					slog.String(obs.KeyAggregateID, aggregateID),
					slog.String(taskIDKey, taskID),
					slog.String(obs.KeyOutcome, outcomeFor(err, retry, maxRetry)),
					slog.Float64(obs.KeyDurationMS, float64(time.Since(start).Nanoseconds())/1e6),
					slog.String("error", err.Error()),
				)
			}
			return err
		})
	}
}

// outcomeFor labels a failed handler for the log: dropped when the error skips
// retry (a decode/parse fault archives the task) or the last attempt is exhausted;
// retryable otherwise (asynq will redeliver with backoff). retry is the 0-based
// attempt count, so retry >= maxRetry means this was the final try.
func outcomeFor(err error, retry, maxRetry int) string {
	if errors.Is(err, asynq.SkipRetry) || retry >= maxRetry {
		return obs.OutcomeDropped
	}
	return obs.OutcomeRetryable
}

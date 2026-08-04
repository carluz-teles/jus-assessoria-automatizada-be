// Package obs is the shared observability vocabulary and span helpers every
// slice and binary reuses, so traces and logs line up on the SAME attribute
// names in the backend (New Relic). It carries no business rule — just the
// standard keys, the tracer under one scope, and the span status convention.
//
// Division of labor (see lib/events consumer middleware): infra code (the asynq
// middleware, the relay) sets the GENERIC dimension — event type, queue, retry,
// outcome, duration; domain code (the use cases) enriches the active span and
// its failure logs with DOMAIN keys it has typed access to — tenant, aggregate,
// window. The golden rule: every failure log carries enough correlation keys to
// find it — tenant_id + the flow's entity id + event_type + trace_id (the last
// stamped automatically by lib/telemetry when a span is active).
package obs

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/jusassessoria/platform/lib/telemetry"
)

// Standard log attribute keys — snake_case to match the infra keys already in
// use (trace_id, span_id, request_id, tenant_id, duration_ms). These add the
// async/event dimension so a slice never invents a variant (the old code used
// "type" for two different things). Use with slog.String(obs.KeyEventType, …).
const (
	KeyEventType   = "event_type"
	KeyEventID     = "event_id"
	KeyAggregateID = "aggregate_id"
	KeyTenantID    = "tenant_id"
	KeyQueue       = "queue"
	KeyRetry       = "retry"
	KeyMaxRetry    = "max_retry"
	KeyOutcome     = "outcome"
	KeyDurationMS  = "duration_ms"
)

// Outcome values for KeyOutcome — low-cardinality and stable so the log message
// template stays groupable while the outcome is a filterable facet.
const (
	OutcomeOK        = "ok"        // handled successfully
	OutcomeRetryable = "retryable" // failed; asynq will redeliver
	OutcomeDropped   = "dropped"   // failed and archived (SkipRetry / decode fault)
)

// Tracer returns the process tracer under the shared instrumentation scope, so
// every span this codebase opens reports one scope name in the backend. It
// reads the global TracerProvider that telemetry.Setup installs; before Setup
// it is a no-op tracer, so calling it at any boot stage is safe.
func Tracer() trace.Tracer {
	return otel.Tracer(telemetry.InstrumentationScope)
}

// Start opens a span from ctx under the shared tracer and returns the derived
// context (carrying the span) and the span. The caller MUST `defer span.End()`.
// It is a thin wrapper so callers never reach for otel.Tracer with an ad-hoc
// scope string.
func Start(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	return Tracer().Start(ctx, name, opts...)
}

// Record sets the span's terminal status from err WITHOUT ending it: on error it
// records the exception AND sets Error status — RecordError alone does not set
// the status (OTel SDK) — otherwise it marks Ok. Pair it with `defer span.End()`
// (typically via a named return: `defer func() { obs.Record(span, err); span.End() }()`).
func Record(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return
	}
	span.SetStatus(codes.Ok, "")
}

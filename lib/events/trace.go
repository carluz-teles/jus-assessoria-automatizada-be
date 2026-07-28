package events

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// traceparentKey is the W3C header the propagator reads and writes. It is the one
// field we carry across the async hop: the relay copies it onto the asynq task so
// the worker's span becomes a child of the producer's (docs erd-backend §4c, §6).
const traceparentKey = "traceparent"

// TraceContextFromCtx serializes the active span to a W3C traceparent using the
// process-global propagator (installed by lib/telemetry.Setup). It returns "" when
// ctx carries no span or no propagator is set — Publish stores that empty string
// and the hop simply begins a fresh trace at the consumer.
func TraceContextFromCtx(ctx context.Context) string {
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	return carrier[traceparentKey]
}

// CtxWithTraceContext rebuilds the producer's trace context from a stored
// traceparent so the consumer continues the same trace. An empty tc is a no-op:
// the caller's ctx is returned unchanged.
func CtxWithTraceContext(ctx context.Context, tc string) context.Context {
	if tc == "" {
		return ctx
	}
	carrier := propagation.MapCarrier{traceparentKey: tc}
	return otel.GetTextMapPropagator().Extract(ctx, carrier)
}

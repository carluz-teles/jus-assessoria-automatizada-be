package events

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

// A valid SpanContext survives Inject → Extract with its trace id and span id
// intact: the consumer's span will attach to the producer's trace.
func TestTraceContext_RoundTrip(t *testing.T) {
	sc := spanContext(t)
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	tc := TraceContextFromCtx(ctx)
	if tc != wantTraceparent {
		t.Fatalf("TraceContextFromCtx() = %q, want %q", tc, wantTraceparent)
	}

	got := trace.SpanContextFromContext(CtxWithTraceContext(context.Background(), tc))
	if got.TraceID() != sc.TraceID() {
		t.Errorf("TraceID = %v, want %v", got.TraceID(), sc.TraceID())
	}
	if got.SpanID() != sc.SpanID() {
		t.Errorf("SpanID = %v, want %v", got.SpanID(), sc.SpanID())
	}
}

// No active span → empty traceparent; the hop then starts a fresh trace.
func TestTraceContextFromCtx_NoSpan(t *testing.T) {
	if tc := TraceContextFromCtx(context.Background()); tc != "" {
		t.Errorf("TraceContextFromCtx() = %q, want empty", tc)
	}
}

// An empty traceparent is a no-op: the caller's ctx is returned unchanged.
func TestCtxWithTraceContext_Empty(t *testing.T) {
	ctx := context.Background()
	if got := CtxWithTraceContext(ctx, ""); got != ctx {
		t.Error("CtxWithTraceContext(ctx, \"\") returned a different context")
	}
}

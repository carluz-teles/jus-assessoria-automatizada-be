package telemetry

import (
	"context"
	"io"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// Attribute keys for the correlation ids injected into every record emitted
// within an active span. Backends match logs to traces on these exact keys.
const (
	traceIDKey = "trace_id"
	spanIDKey  = "span_id"
)

// TraceHandler decorates a slog.Handler, stamping the active span's trace_id
// and span_id onto each record so logs correlate with traces. When the context
// carries no valid span, it is a transparent pass-through.
//
// Security: this handler only ever adds correlation ids — it never inspects or
// copies attribute values. Never log secrets or PII: keep sensitive values out
// of the records you emit; nothing here scrubs them.
type TraceHandler struct {
	base slog.Handler
}

// compile-time check: TraceHandler satisfies the handler interface it wraps.
var _ slog.Handler = (*TraceHandler)(nil)

// NewTraceHandler wraps base so records gain trace correlation ids.
func NewTraceHandler(base slog.Handler) *TraceHandler {
	return &TraceHandler{base: base}
}

// Enabled delegates to the wrapped handler.
func (h *TraceHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.base.Enabled(ctx, level)
}

// Handle adds trace_id/span_id when ctx carries a valid span, then delegates.
// The record is cloned before mutation because slog may share its backing
// storage across handlers (see slog.Handler contract).
func (h *TraceHandler) Handle(ctx context.Context, rec slog.Record) error {
	sc := trace.SpanContextFromContext(ctx)
	if sc.IsValid() {
		rec = rec.Clone()
		rec.AddAttrs(
			slog.String(traceIDKey, sc.TraceID().String()),
			slog.String(spanIDKey, sc.SpanID().String()),
		)
	}
	return h.base.Handle(ctx, rec)
}

// WithAttrs returns a TraceHandler wrapping the delegated base so trace
// injection survives across derived loggers.
func (h *TraceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &TraceHandler{base: h.base.WithAttrs(attrs)}
}

// WithGroup returns a TraceHandler wrapping the delegated base so trace
// injection survives across derived loggers.
func (h *TraceHandler) WithGroup(name string) slog.Handler {
	return &TraceHandler{base: h.base.WithGroup(name)}
}

// NewLogger returns a JSON logger writing to w at the given level, wrapped in a
// TraceHandler so every record emitted within a span carries trace_id/span_id.
// Never log secrets or PII (see TraceHandler).
func NewLogger(w io.Writer, level slog.Leveler) *slog.Logger {
	base := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})
	return slog.New(NewTraceHandler(base))
}

// SetupDefault builds a trace-correlating JSON logger and installs it as
// slog.Default, then returns it. Call once at boot so package-level slog calls
// carry trace context. Never log secrets or PII (see TraceHandler).
func SetupDefault(w io.Writer, level slog.Leveler) *slog.Logger {
	logger := NewLogger(w, level)
	slog.SetDefault(logger)
	return logger
}

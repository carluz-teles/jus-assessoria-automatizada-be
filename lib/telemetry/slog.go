package telemetry

import (
	"context"
	"errors"
	"io"
	"log/slog"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/trace"
)

// Attribute keys for the correlation ids injected into every record emitted
// within an active span. Backends match logs to traces on these exact keys.
const (
	traceIDKey = "trace_id"
	spanIDKey  = "span_id"
)

// InstrumentationScope names this module as the instrumentation scope on the
// OTel log bridge AND the tracer (lib/obs), so records and spans exported via
// OTLP carry one stable scope name across every binary.
const InstrumentationScope = "github.com/jusassessoria/platform"

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

// fanoutHandler dispatches every record to several base handlers. It backs the
// dual-handler logger: the same log line goes to stdout JSON (for Railway/debug)
// AND to the OTel bridge (for OTLP export), so New Relic correlates logs with
// traces on the trace_id/span_id the stdout side already stamps.
//
// It never clones or mutates the record: the slog.Handler contract forbids
// Handle from modifying the record it receives, and each base that needs to
// mutate (TraceHandler) clones on its own. The same record is passed to every
// base unchanged.
type fanoutHandler struct {
	bases []slog.Handler
}

// compile-time check: fanoutHandler satisfies the handler interface.
var _ slog.Handler = (*fanoutHandler)(nil)

// newFanoutHandler aggregates bases into a single handler that fans out to all.
func newFanoutHandler(bases ...slog.Handler) *fanoutHandler {
	return &fanoutHandler{bases: bases}
}

// Enabled reports true as soon as ANY base is enabled for the level, so no base
// misses a record it would have processed. Handle re-checks each base, so a
// level a subset of bases care about still reaches only those bases.
func (h *fanoutHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, b := range h.bases {
		if b.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

// Handle delivers the same record to every base that is Enabled for its level
// and joins their errors, so one base failing neither hides another's failure
// nor stops the others. The per-base Enabled guard preserves each base's own
// level filtering even though the outer logger only checked the OR (Enabled).
func (h *fanoutHandler) Handle(ctx context.Context, rec slog.Record) error {
	var errs []error
	for _, b := range h.bases {
		if !b.Enabled(ctx, rec.Level) {
			continue
		}
		if err := b.Handle(ctx, rec); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// WithAttrs returns a fanout whose bases each carry attrs, so derived loggers
// keep fanning out to every destination.
func (h *fanoutHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	bases := make([]slog.Handler, len(h.bases))
	for i, b := range h.bases {
		bases[i] = b.WithAttrs(attrs)
	}
	return &fanoutHandler{bases: bases}
}

// WithGroup returns a fanout whose bases each opened the group, so derived
// loggers keep fanning out to every destination.
func (h *fanoutHandler) WithGroup(name string) slog.Handler {
	bases := make([]slog.Handler, len(h.bases))
	for i, b := range h.bases {
		bases[i] = b.WithGroup(name)
	}
	return &fanoutHandler{bases: bases}
}

// NewLogger returns a dual-destination JSON logger writing to w at the given
// level. Every record fans out to two handlers: (1) stdout JSON wrapped in a
// TraceHandler, so each record in a span carries trace_id/span_id (unchanged);
// and (2) an OTel bridge that exports the record over OTLP.
//
// The bridge uses the GLOBAL LoggerProvider (no WithLoggerProvider): it is a
// no-op until Setup calls global.SetLoggerProvider, then begins emitting. This
// late delegation is deliberate — the logger is built at the boot logger step,
// before Setup installs the provider, and must not force a boot reorder.
//
// Never log secrets or PII (see TraceHandler).
func NewLogger(w io.Writer, level slog.Leveler) *slog.Logger {
	stdout := NewTraceHandler(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level}))
	bridge := otelslog.NewHandler(InstrumentationScope)
	return slog.New(newFanoutHandler(stdout, bridge))
}

// SetupDefault builds a trace-correlating JSON logger and installs it as
// slog.Default, then returns it. Call once at boot so package-level slog calls
// carry trace context. Never log secrets or PII (see TraceHandler).
func SetupDefault(w io.Writer, level slog.Leveler) *slog.Logger {
	logger := NewLogger(w, level)
	slog.SetDefault(logger)
	return logger
}

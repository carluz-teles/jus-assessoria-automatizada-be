package middleware

import (
	otelfiber "github.com/gofiber/contrib/otelfiber/v2"
	"github.com/gofiber/fiber/v2"
)

// Telemetry opens a root span for each request and injects the trace context
// into c.UserContext(). Downstream code — and lib/telemetry's slog handler in
// particular — reads that context to stamp trace_id/span_id onto every log
// record, which is why this runs before Recover and RequestLogger (§4.5). On the
// way out it also injects the W3C traceparent onto the response via the global
// propagator, so a trace continues across the next hop.
//
// It deliberately takes no provider: otelfiber falls back to the global
// TracerProvider and propagator, both installed once at boot by
// telemetry.Setup. Keeping the wiring global means every binary shares one
// configuration and this middleware stays a thin adapter.
//
// The optional X-Trace-Id response header (WithTraceResponseHeader) is a v3-only
// option; the pinned otelfiber v2 does not expose it, so it is omitted. Trace ⇄
// log correlation does not depend on it — it rides on c.UserContext().
func Telemetry() fiber.Handler {
	return otelfiber.Middleware()
}

package middleware

import (
	"log/slog"

	"github.com/gofiber/fiber/v2"
)

// Base returns the base middleware chain in the exact order of execution fixed
// by docs/erd-backend.md §4.5. Register it with:
//
//	for _, h := range middleware.Base(logger) {
//	    app.Use(h)
//	}
//
// Order is load-bearing:
//
//   - RequestID first, so a correlation id exists for everything that follows.
//   - Telemetry next, so the trace context is live before any log is written —
//     trace_id/span_id must already be on the context for RequestLogger (and any
//     handler log) to carry them.
//   - Recover early, so it wraps — and therefore catches panics from — every
//     middleware and handler after it, including Auth and RequestLogger.
//   - RequestLogger last, so it observes the final status the rest of the chain
//     produced.
//
// Auth (Clerk, a later slice) slots between Recover and RequestLogger: after
// Recover so a panic in token verification is caught, before RequestLogger so
// the access log can carry the resolved tenant. It is not built here.
func Base(logger *slog.Logger) []fiber.Handler {
	return []fiber.Handler{
		RequestID(),
		Telemetry(),
		Recover(),
		RequestLogger(logger),
	}
}

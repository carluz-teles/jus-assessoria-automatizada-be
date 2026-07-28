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

// WithAuth returns the base chain with the Clerk Auth handler spliced into its
// documented position — after Recover (so a panic in token verification is
// caught) and before RequestLogger (so the access log carries the resolved
// tenant), per §4.5. Base stays untouched; the api binary composes the two:
//
//	chain := middleware.WithAuth(middleware.Base(logger), middleware.Auth(v, r))
//	for _, h := range chain {
//	    app.Use(h)
//	}
//
// RequestLogger is always the last handler Base emits, so Auth is inserted just
// before it. Passing an empty base yields a chain of Auth alone.
func WithAuth(base []fiber.Handler, auth fiber.Handler) []fiber.Handler {
	if len(base) == 0 {
		return []fiber.Handler{auth}
	}

	last := len(base) - 1
	chain := make([]fiber.Handler, 0, len(base)+1)
	chain = append(chain, base[:last]...)
	chain = append(chain, auth)
	chain = append(chain, base[last])
	return chain
}

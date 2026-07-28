// Package middleware holds the base HTTP middleware chain for the api binary:
// request-id propagation, OpenTelemetry tracing, panic recovery and structured
// request logging. It sits at the HTTP edge (lib/httpx) and depends only on
// lib/apperr, lib/httpx and lib/telemetry — never on a domain slice.
//
// The handlers are composed by Base (chain.go) in the order docs/erd-backend.md
// §4.5 fixes. Auth (Clerk) is a later slice and slots between Recover and
// RequestLogger; nothing here needs to change when it lands.
package middleware

import (
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
)

const (
	// requestIDHeader is the inbound and outbound header carrying the id. We
	// honour a client-supplied value so a caller can correlate its own logs
	// with ours across the hop.
	requestIDHeader = "X-Request-ID"
	// requestIDLocal is the request-local key under which the id is stored for
	// downstream handlers and RequestLogger.
	requestIDLocal = "request_id"
)

// RequestID propagates a per-request correlation id. It reads X-Request-ID from
// the incoming request and, when absent, mints a fresh UUID. The id is stored on
// c.Locals for downstream use and echoed on the X-Request-ID response header so
// the caller sees the value that ties back to our logs.
func RequestID() fiber.Handler {
	return func(c *fiber.Ctx) error {
		id := c.Get(requestIDHeader)
		if id == "" {
			id = uuid.NewString()
		}

		c.Locals(requestIDLocal, id)
		c.Set(requestIDHeader, id)

		return c.Next()
	}
}

// RequestIDFromCtx returns the request id set by RequestID, or "" when the
// middleware did not run for this request.
func RequestIDFromCtx(c *fiber.Ctx) string {
	id, _ := c.Locals(requestIDLocal).(string)
	return id
}

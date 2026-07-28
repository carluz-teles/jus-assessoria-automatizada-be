package middleware

import (
	"fmt"
	"runtime/debug"

	"github.com/gofiber/fiber/v2"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/httpx"
)

// Recover turns any panic raised by a downstream handler or middleware into a
// typed 500 instead of letting it crash the process. Because it wraps the rest
// of the chain, it also catches panics in the middlewares that follow it (Auth,
// RequestLogger) — the reason §4.5 places it early.
//
// The recovered value and its stack ride in the cause of an INFRA AppError, so
// httpx.WriteError emits a single trace-correlated log line (with the stack in
// the "cause" attribute) and returns the generic {kind,message} envelope. The
// cause is logged, never serialized to the client — the stack cannot leak. One
// log line, via slog, honours the single-handling rule.
func Recover() fiber.Handler {
	return func(c *fiber.Ctx) (err error) {
		defer func() {
			r := recover()
			if r == nil {
				return
			}

			// Capture the stack at the recovery point; it stays inside the
			// cause and reaches the logs through WriteError, never the client.
			cause := fmt.Errorf("panic: %v\n%s", r, debug.Stack())
			err = httpx.WriteError(c, apperr.NewInfra("internal error", cause))
		}()

		return c.Next()
	}
}

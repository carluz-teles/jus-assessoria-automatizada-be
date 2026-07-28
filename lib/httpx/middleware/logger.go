package middleware

import (
	"log/slog"
	"time"

	"github.com/gofiber/fiber/v2"
)

// RequestLogger emits one structured line per request after the chain runs:
// method, path, status, duration and request id. trace_id/span_id are added
// automatically by lib/telemetry's handler when logger is the trace-correlating
// logger and the record is emitted with c.UserContext() — which carries the span
// opened by Telemetry. Pass that logger (telemetry.NewLogger) so the correlation
// lands.
//
// It logs c.Path(), which excludes the query string, so ids or tokens smuggled
// in query params never reach the logs. Nothing else logged here is PII.
func RequestLogger(logger *slog.Logger) fiber.Handler {
	return func(c *fiber.Ctx) error {
		start := time.Now()

		err := c.Next()

		durationMS := float64(time.Since(start).Nanoseconds()) / 1e6
		logger.LogAttrs(
			c.UserContext(),
			slog.LevelInfo,
			"http request",
			slog.String("method", c.Method()),
			slog.String("path", c.Path()),
			slog.Int("status", c.Response().StatusCode()),
			slog.Float64("duration_ms", durationMS),
			slog.String("request_id", RequestIDFromCtx(c)),
		)

		return err
	}
}

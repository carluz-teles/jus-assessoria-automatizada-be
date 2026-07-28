package middleware_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/jusassessoria/platform/lib/httpx/middleware"
	"github.com/jusassessoria/platform/lib/telemetry"
)

// TestBase_ChainPassesThrough is a light smoke test of the full chain — including
// Telemetry against the global no-op tracer (telemetry.Setup is not called in the
// unit context). It asserts a normal request reaches the handler, returns 200 and
// carries the X-Request-ID response header the chain adds. A full span assertion
// belongs to an integration test with a real exporter.
func TestBase_ChainPassesThrough(t *testing.T) {
	t.Parallel()

	logger := telemetry.NewLogger(io.Discard, slog.LevelInfo)

	app := fiber.New()
	for _, h := range middleware.Base(logger) {
		app.Use(h)
	}
	app.Get("/v1/ping", func(c *fiber.Ctx) error {
		return c.SendString("pong")
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/v1/ping", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != "pong" {
		t.Errorf("body = %q, want pong", body)
	}

	if resp.Header.Get("X-Request-ID") == "" {
		t.Error("X-Request-ID response header is empty")
	}
}

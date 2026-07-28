package middleware_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/jusassessoria/platform/lib/httpx/middleware"
	"github.com/jusassessoria/platform/lib/telemetry"
)

func TestRequestLogger_EmitsStructuredLine(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	// The production trace-correlating logger, pointed at a buffer so the test
	// can inspect the emitted record.
	logger := telemetry.NewLogger(&buf, slog.LevelInfo)

	app := fiber.New()
	// RequestID first so the log line carries a request_id, as in the real chain.
	app.Use(middleware.RequestID())
	app.Use(middleware.RequestLogger(logger))
	app.Get("/v1/things", func(c *fiber.Ctx) error {
		return c.SendStatus(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/things", nil)
	req.Header.Set("X-Request-ID", "req-log-1")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	resp.Body.Close()

	var rec struct {
		Msg        string  `json:"msg"`
		Method     string  `json:"method"`
		Path       string  `json:"path"`
		Status     int     `json:"status"`
		DurationMS float64 `json:"duration_ms"`
		RequestID  string  `json:"request_id"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("decode log line %q: %v", buf.String(), err)
	}

	if rec.Method != http.MethodGet {
		t.Errorf("method = %q, want GET", rec.Method)
	}
	if rec.Path != "/v1/things" {
		t.Errorf("path = %q, want /v1/things", rec.Path)
	}
	if rec.Status != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Status)
	}
	if rec.RequestID != "req-log-1" {
		t.Errorf("request_id = %q, want req-log-1", rec.RequestID)
	}
}

// TestRequestLogger_OmitsQueryString guards the PII rule: the query string, which
// may carry ids or tokens, must not appear in the logged path.
func TestRequestLogger_OmitsQueryString(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := telemetry.NewLogger(&buf, slog.LevelInfo)

	app := fiber.New()
	app.Use(middleware.RequestLogger(logger))
	app.Get("/v1/things", func(c *fiber.Ctx) error {
		return c.SendStatus(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/things?token=secret-token", nil)

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	resp.Body.Close()

	if bytes.Contains(buf.Bytes(), []byte("secret-token")) {
		t.Errorf("query string leaked into log: %s", buf.String())
	}
}

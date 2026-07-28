package middleware_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/httpx/middleware"
)

func TestRecover_PanicBecomesTypedInternalError(t *testing.T) {
	t.Parallel()

	// WriteError logs the recovered cause on the default logger; silence it so a
	// deliberate panic does not spam the test output. The stack must not reach the
	// client, which the body assertion below proves.
	slog.SetLogLoggerLevel(slog.LevelError + 1)

	const secret = "db password=hunter2 at 10.0.0.1"

	app := fiber.New()
	app.Use(middleware.Recover())
	app.Get("/boom", func(*fiber.Ctx) error {
		panic(secret)
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/boom", nil))
	if err != nil {
		// A crash here would mean Recover let the panic escape — the process
		// (test binary) would abort rather than return this error.
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	var body struct {
		Kind    string `json:"kind"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode body %q: %v", raw, err)
	}

	if body.Kind != string(apperr.KindInfra) {
		t.Errorf("kind = %q, want %q", body.Kind, apperr.KindInfra)
	}
	if body.Message != "internal error" {
		t.Errorf("message = %q, want generic %q", body.Message, "internal error")
	}
	// The panic value (a stand-in for a secret) must never leak into the body.
	if strings.Contains(string(raw), "hunter2") || strings.Contains(string(raw), "10.0.0.1") {
		t.Errorf("panic cause leaked into body: %s", raw)
	}
}

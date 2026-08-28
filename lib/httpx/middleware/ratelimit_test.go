package middleware_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/httpx/middleware"
)

// rateLimitedBody mirrors httpx.ErrorBody, decoded independently so the test
// asserts on the wire shape rather than reusing the producing package's type.
type rateLimitedBody struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

func newRateLimitedApp(max int, window time.Duration) *fiber.App {
	app := fiber.New()
	app.Use(middleware.RateLimit(max, window))
	app.Get("/v1/things", func(c *fiber.Ctx) error {
		return c.SendStatus(http.StatusOK)
	})
	return app
}

// TestRateLimit_NPlusOneReturns429WithEnvelope proves the (max+1)th request
// inside the window is rejected with the project's standard {kind,message}
// envelope, Kind RATE_LIMITED, and that the first max requests still pass.
func TestRateLimit_NPlusOneReturns429WithEnvelope(t *testing.T) {
	t.Parallel()

	const max = 2
	app := newRateLimitedApp(max, time.Minute)

	for i := 0; i < max; i++ {
		resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/v1/things", nil))
		if err != nil {
			t.Fatalf("app.Test (request %d): %v", i+1, err)
		}
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d status = %d, want 200 (within budget)", i+1, resp.StatusCode)
		}
	}

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/v1/things", nil))
	if err != nil {
		t.Fatalf("app.Test (over budget): %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	var body rateLimitedBody
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode body %q: %v", raw, err)
	}

	if body.Kind != string(apperr.KindRateLimited) {
		t.Errorf("kind = %q, want %q", body.Kind, apperr.KindRateLimited)
	}
	if body.Message == "" {
		t.Error("message is empty")
	}
}

// TestRateLimit_ResetsAfterWindow proves a caller over budget is allowed
// through again once the fixed window rolls over. Fiber's fixed-window
// limiter tracks time at one-second granularity (it rounds Expiration up to
// whole seconds internally), so 1s is the shortest window that actually
// exercises a reset — the sleep below is short in absolute terms even though
// it is not sub-second.
func TestRateLimit_ResetsAfterWindow(t *testing.T) {
	t.Parallel()

	const (
		max    = 1
		window = time.Second
	)
	app := newRateLimitedApp(max, window)

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/v1/things", nil))
	if err != nil {
		t.Fatalf("app.Test (first): %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", resp.StatusCode)
	}

	resp, err = app.Test(httptest.NewRequest(http.MethodGet, "/v1/things", nil))
	if err != nil {
		t.Fatalf("app.Test (over budget): %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("over-budget status = %d, want 429", resp.StatusCode)
	}

	time.Sleep(window + 200*time.Millisecond)

	resp, err = app.Test(httptest.NewRequest(http.MethodGet, "/v1/things", nil))
	if err != nil {
		t.Fatalf("app.Test (after window): %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("after-window status = %d, want 200 (budget reset)", resp.StatusCode)
	}
}

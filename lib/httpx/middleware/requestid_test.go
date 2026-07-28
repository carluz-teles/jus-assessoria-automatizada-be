package middleware_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/jusassessoria/platform/lib/httpx/middleware"
)

// echoRequestID mounts RequestID and returns the id seen on c.Locals as the body,
// so a test can assert the Locals value, the response header and the echoed input
// all agree.
func echoRequestID(t *testing.T, req *http.Request) (*http.Response, string) {
	t.Helper()

	app := fiber.New()
	app.Use(middleware.RequestID())
	app.Get("/", func(c *fiber.Ctx) error {
		return c.SendString(middleware.RequestIDFromCtx(c))
	})

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	return resp, string(body)
}

func TestRequestID_GeneratesWhenAbsent(t *testing.T) {
	t.Parallel()

	resp, locals := echoRequestID(t, httptest.NewRequest(http.MethodGet, "/", nil))

	header := resp.Header.Get("X-Request-ID")
	if header == "" {
		t.Fatal("X-Request-ID response header is empty, want a generated id")
	}
	// The generated id must be the same one downstream handlers read from Locals.
	if header != locals {
		t.Errorf("header id %q != locals id %q", header, locals)
	}
}

func TestRequestID_EchoesProvided(t *testing.T) {
	t.Parallel()

	const provided = "req-abc-123"

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-ID", provided)

	resp, locals := echoRequestID(t, req)

	if got := resp.Header.Get("X-Request-ID"); got != provided {
		t.Errorf("header id = %q, want the provided %q", got, provided)
	}
	if locals != provided {
		t.Errorf("locals id = %q, want the provided %q", locals, provided)
	}
}

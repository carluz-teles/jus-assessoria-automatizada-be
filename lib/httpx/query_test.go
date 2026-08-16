package httpx

import (
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
)

// A query param outside the route's allowlist is a client error → 400, never ignored
// (docs/erd-backend.md §4e.3: "Filtro fora da lista → 400, não é ignorado silenciosamente").
func TestRejectUnknownParams_RejectsUnknown(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	app.Get("/", func(c *fiber.Ctx) error {
		if err := RejectUnknownParams(c, map[string]struct{}{"limit": {}, "cursor": {}}); err != nil {
			return WriteError(c, err)
		}
		return c.SendStatus(fiber.StatusOK)
	})

	req := httptest.NewRequest("GET", "/?limit=20&sort=-created", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Errorf("status = %d, want 400 for unknown ?sort", resp.StatusCode)
	}
}

// Every param in the allowlist passes through; the handler runs.
func TestRejectUnknownParams_AllowsKnown(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	app.Get("/", func(c *fiber.Ctx) error {
		if err := RejectUnknownParams(c, map[string]struct{}{"limit": {}, "cursor": {}, "search": {}}); err != nil {
			return WriteError(c, err)
		}
		return c.SendStatus(fiber.StatusOK)
	})

	for _, q := range []string{"/", "/?limit=20", "/?limit=20&cursor=abc&search=petrobras"} {
		req := httptest.NewRequest("GET", q, nil)
		resp, err := app.Test(req)
		if err != nil {
			t.Fatalf("app.Test(%s): %v", q, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != fiber.StatusOK {
			t.Errorf("GET %s: status = %d, want 200", q, resp.StatusCode)
		}
	}
}

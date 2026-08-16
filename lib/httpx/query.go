package httpx

import (
	"github.com/gofiber/fiber/v2"

	"github.com/jusassessoria/platform/lib/apperr"
)

// RejectUnknownParams enforces a route's query-param allowlist (docs/erd-backend.md
// §4e.3): a param outside the list is a client error → 400, never ignored silently.
// Each list handler declares its own allowed set and calls this first; everything the
// handler reads (limit, cursor, the search term, every filter) must be listed.
func RejectUnknownParams(c *fiber.Ctx, allowed map[string]struct{}) error {
	for key := range c.Queries() {
		if _, ok := allowed[key]; !ok {
			return apperr.NewInvalid("unknown query parameter: " + key)
		}
	}
	return nil
}

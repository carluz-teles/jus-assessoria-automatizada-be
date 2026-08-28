package middleware

import (
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/limiter"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/httpx"
)

// RateLimit returns a fixed-window, per-IP rate limiter built on Fiber's own
// limiter middleware (already part of the pinned gofiber/fiber/v2 module — no
// new dependency). max is how many requests a single IP (KeyGenerator defaults
// to c.IP()) may make within window; the rest are rejected with 429 and the
// project's standard {kind,message,details} envelope (§4.4), Kind
// apperr.KindRateLimited.
//
// It is NOT part of middleware.Base: the two traffic classes this api exposes
// need different budgets keyed on path, and Base has no path awareness (it
// runs before routing decides which group a request belongs to). cmd/api's
// router mounts one instance scoped to /v1 (generous — many users legitimately
// share one office's public IP behind a NAT) and a second, tighter one scoped
// to /webhooks (Clerk/Stripe/Resend: low, predictable volume, so it must not
// share budget with real user traffic). See router.go for exactly where each
// is mounted and why.
//
// The store is Fiber's default in-memory map: per api process, not shared
// across replicas, so the effective ceiling scales with instance count. That
// is an accepted trade-off for a first line of defense against obvious
// volumetric abuse — a shared store (e.g. Redis) is the upgrade if that
// becomes a problem.
func RateLimit(max int, window time.Duration) fiber.Handler {
	return limiter.New(limiter.Config{
		Max:        max,
		Expiration: window,
		LimitReached: func(c *fiber.Ctx) error {
			return httpx.WriteError(c, apperr.NewRateLimited("too many requests"))
		},
	})
}

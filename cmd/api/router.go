package main

import (
	"log/slog"

	"github.com/gofiber/fiber/v2"

	"github.com/jusassessoria/platform/internal/identity"
	"github.com/jusassessoria/platform/lib/httpx"
	"github.com/jusassessoria/platform/lib/httpx/middleware"
)

// routerDeps carries everything newRouter wires into the Fiber app. Assembling
// the graph here (not inside newRouter) keeps the router a pure function of its
// dependencies — the test builds it with fakes and never touches the network.
type routerDeps struct {
	logger   *slog.Logger
	verifier middleware.TokenVerifier
	resolver middleware.PrincipalResolver
	webhook  *identity.WebhookHandler
}

// newRouter builds the api's Fiber app: the global middleware chain, the two
// public entry points (health probe, Clerk webhook) and the authenticated /v1
// group. It performs no I/O, so it is the seam the router test exercises.
//
// The base chain (RequestID → Telemetry → Recover → RequestLogger, §4.5) applies
// to every route. Auth is NOT global: /health must answer without a token and
// the webhook verifies its own svix signature, so authentication guards only the
// /v1 group.
func newRouter(deps routerDeps) *fiber.App {
	app := fiber.New(fiber.Config{
		// Any error a handler returns instead of writing itself still flows
		// through the single {kind,message,details} envelope (§4.4).
		ErrorHandler: func(c *fiber.Ctx, err error) error {
			return httpx.WriteError(c, err)
		},
		DisableStartupMessage: true,
	})

	for _, h := range middleware.Base(deps.logger) {
		app.Use(h)
	}

	// Public: liveness probe. The readiness probe that reports per-dependency
	// status arrives with the docker slice; here a fixed 200 is enough for load
	// balancers to route traffic.
	app.Get("/health", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{"status": "ok"})
	})

	// Public: Clerk provisioning webhook. It is unauthenticated at the router
	// level because the handler verifies the svix signature itself (§4d.3).
	app.Post("/webhooks/clerk", deps.webhook.Handle)

	// Authenticated surface. Auth verifies the JWT and resolves the internal
	// Principal before any /v1 handler runs; the placeholder ping proves the
	// group is wired — feature slices hang their routes off it.
	v1 := app.Group("/v1", middleware.Auth(deps.verifier, deps.resolver))
	v1.Get("/ping", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{"status": "ok"})
	})

	return app
}

package main

import (
	"log/slog"
	"strings"

	"github.com/gofiber/fiber/v2"

	"github.com/jusassessoria/platform/internal/acquisition"
	"github.com/jusassessoria/platform/internal/billing"
	"github.com/jusassessoria/platform/internal/identity"
	"github.com/jusassessoria/platform/internal/lookup"
	"github.com/jusassessoria/platform/internal/notifications"
	"github.com/jusassessoria/platform/lib/httpx"
	"github.com/jusassessoria/platform/lib/httpx/middleware"
)

// lookupPrefix is the onboarding subtree that authenticates WITHOUT a tenant
// (AuthUser). Every other /v1 route needs a resolved Principal; this is the one
// explicit exemption — secure by default. The trailing slash pins it to the
// lookup routes and never a sibling that merely starts with "lookup".
const lookupPrefix = "/v1/lookup/"

// identityMePath is the other tenant-less onboarding route: the wizard reads it
// before an escritório exists, so it runs under AuthUser like the lookup subtree.
// Matched exactly (it is a single route, not a subtree) so no sibling is exempted.
const identityMePath = "/v1/identity/me"

// routerDeps carries everything newRouter wires into the Fiber app. Assembling
// the graph here (not inside newRouter) keeps the router a pure function of its
// dependencies — the test builds it with fakes and never touches the network.
type routerDeps struct {
	logger               *slog.Logger
	corsOrigins          string
	verifier             middleware.TokenVerifier
	resolver             middleware.PrincipalResolver
	webhook              *identity.WebhookHandler
	billingWebhook       *billing.WebhookHandler
	notificationsWebhook *notifications.WebhookHandler
	billing              *billing.Handler
	identity             *identity.Handler
	acquisition          *acquisition.Handler
	lookup               *lookup.Handler
}

// newRouter builds the api's Fiber app: the global middleware chain, the two
// public entry points (health probe, Clerk webhook) and the authenticated /v1
// group. It performs no I/O, so it is the seam the router test exercises.
//
// The base chain (RequestID → Telemetry → Recover → RequestLogger, §4.5) applies
// to every route. Auth is NOT global: /health must answer without a token and
// the webhook verifies its own svix signature, so authentication guards only the
// /v1 group.
//
// Two auth regimes live under /v1. Fiber matches group middleware by path prefix,
// so a blanket Use("/v1", Auth) would also cover /v1/lookup and 401 a tenant-less
// onboarding user. A single dispatch middleware on the /v1 group therefore routes
// the onboarding lookup subtree through AuthUser (verified user, no tenant) and
// everything else through Auth (verified user + resolved tenant).
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

	// CORS: the FE calls the api cross-origin with a bearer token (a non-simple
	// request), so the browser preflights and blocks any response missing
	// Access-Control-Allow-Origin. Mounted here — after Base, before the /v1 auth
	// dispatch below — so the unauthenticated OPTIONS preflight is answered (204),
	// not 401'd, and the CORS headers ride on every /v1 response.
	app.Use(middleware.CORS(deps.corsOrigins))

	// Public: liveness probe. The readiness probe that reports per-dependency
	// status arrives with the docker slice; here a fixed 200 is enough for load
	// balancers to route traffic.
	app.Get("/health", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{"status": "ok"})
	})

	// Domain routes come from each slice, never hand-listed here — the api only
	// composes. identity owns its public Clerk provisioning webhook (svix-verified
	// inside, so unauthenticated at the router level, §4d.3) and mounts it via
	// Register. Adding a domain = one more slice.Register(...) call, no route here.
	deps.webhook.Register(app)

	// billing owns its public Stripe webhook (svix-analog: the Stripe signature is
	// verified inside the use case, so unauthenticated at the router level) and
	// mounts it via Register. Nil-guarded so the router test fixture builds without
	// a use case, like the others.
	if deps.billingWebhook != nil {
		deps.billingWebhook.Register(app)
	}

	// notifications owns its public Resend webhook (bounce/complaint), verified by
	// its svix signature inside the handler, so unauthenticated at the router level.
	// Mounted via Register and nil-guarded so the router test fixture builds without
	// a use case, like the others.
	if deps.notificationsWebhook != nil {
		deps.notificationsWebhook.Register(app)
	}

	// Authenticated surface. The dispatch middleware runs ahead of every /v1
	// route (registered before them), picking the right auth by subtree.
	tenantAuth := middleware.Auth(deps.verifier, deps.resolver)
	userAuth := middleware.AuthUser(deps.verifier)

	v1 := app.Group("/v1")
	v1.Use(func(c *fiber.Ctx) error {
		if strings.HasPrefix(c.Path(), lookupPrefix) || c.Path() == identityMePath {
			return userAuth(c)
		}
		return tenantAuth(c)
	})

	// The placeholder ping proves the tenant-authenticated group is wired —
	// feature slices mount their routes here via RegisterV1.
	v1.Get("/ping", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{"status": "ok"})
	})

	// identity owns its onboarding /v1 routes and mounts them via RegisterMe (the
	// tenant-less /identity/me, routed through AuthUser by the dispatch above) and
	// RegisterV1 (the tenant-strict /organization/profile). Nil-guarded like the
	// others so the router test fixture builds without a use case.
	if deps.identity != nil {
		deps.identity.RegisterMe(v1)
		deps.identity.RegisterV1(v1)
	}

	// acquisition owns its /v1 routes and mounts them via RegisterV1 — the api
	// only composes. Nil-guarded so the router test's fixture (which wires no
	// domain use cases) still builds.
	if deps.acquisition != nil {
		deps.acquisition.RegisterV1(v1)
	}

	// billing owns its authenticated /v1 routes (checkout/portal/subscription/plans)
	// and mounts them via RegisterV1 — its public Stripe webhook is a separate
	// handler mounted off the root above. Nil-guarded like the others so the router
	// test fixture builds without a use case.
	if deps.billing != nil {
		deps.billing.RegisterV1(v1)
	}

	// lookup owns the onboarding /v1/lookup routes (tenant-less, AuthUser above)
	// and mounts them via Register — one line of composition, nil-guarded like the
	// others so the router test fixture builds without a registry.
	if deps.lookup != nil {
		deps.lookup.Register(v1)
	}

	return app
}

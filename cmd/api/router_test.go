package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jusassessoria/platform/internal/identity"
	"github.com/jusassessoria/platform/internal/lookup"
	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/httpx"
	"github.com/jusassessoria/platform/lib/telemetry"
)

// testWebhookSecret is svix's documented example signing secret (whsec_ + base64),
// valid in format so NewWebhook succeeds; a request with no svix headers then fails
// verification with 401 — enough to prove the route reaches Handle through Register.
const testWebhookSecret = "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw"

// testCORSOrigin is the single allowed browser origin the router fixture is built
// with — the CORS preflight tests assert the api echoes exactly this origin.
const testCORSOrigin = "https://fe.test"

// fakeVerifier and fakeResolver stand in for the Clerk-backed implementations so
// the router builds without any network or database. The /health test never
// reaches the /v1 group, so their behaviour is irrelevant here — they exist only
// to satisfy newRouter's dependency types.
type fakeVerifier struct{}

func (fakeVerifier) Verify(context.Context, string) (userID, orgID, role string, err error) {
	return "", "", "", nil
}

type fakeResolver struct{}

func (fakeResolver) Resolve(context.Context, string, string) (httpx.Principal, error) {
	return httpx.Principal{}, nil
}

// testRateLimitMax/testRateLimitWebhookMax/testRateLimitWindow mirror the
// production defaults (lib/config's RATE_LIMIT_*) — set explicitly so the
// fixture's behavior does not depend on the limiter package's own zero-value
// fallback (5 req/min), which is high enough to be misread as "no limiter".
const (
	testRateLimitMax        = 600
	testRateLimitWebhookMax = 120
)

var testRateLimitWindow = 60 * time.Second

func newTestRouterDeps() routerDeps {
	return routerDeps{
		logger:              telemetry.NewLogger(io.Discard, nil),
		rateLimitMax:        testRateLimitMax,
		rateLimitWebhookMax: testRateLimitWebhookMax,
		rateLimitWindow:     testRateLimitWindow,
		corsOrigins:         testCORSOrigin,
		verifier:            fakeVerifier{},
		resolver:            fakeResolver{},
		// webhook is never invoked by these tests; a nil handler is safe because
		// no route below reaches Handle. Left nil deliberately to keep the fixture
		// free of a real UseCase.
		webhook: nil,
	}
}

// erroringResolver fails every resolution, standing in for a user whose tenant
// cannot be resolved (the onboarding case). Any route that runs tenant Auth turns
// this into a 401; a route that runs AuthUser never calls it.
type erroringResolver struct{}

func (erroringResolver) Resolve(context.Context, string, string) (httpx.Principal, error) {
	return httpx.Principal{}, apperr.NewNotFound("no tenant for user")
}

// stubRegistry is a lookup.RegistryLookup that always succeeds, so the lookup
// route reaches 200 whenever auth lets it through.
type stubRegistry struct{}

func (stubRegistry) LookupCNPJ(context.Context, string) (lookup.Company, error) {
	return lookup.Company{CNPJ: "19131243000197"}, nil
}

func (stubRegistry) LookupCEP(context.Context, string) (lookup.Address, error) {
	return lookup.Address{CEP: "01311902"}, nil
}

func TestNewRouter_Health_Returns200(t *testing.T) {
	app := newRouter(newTestRouterDeps())

	req := httptest.NewRequest("GET", "/health", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test() error = %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("GET /health status = %d, want 200", resp.StatusCode)
	}

	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf(`GET /health body[status] = %q, want "ok"`, body["status"])
	}
}

// The /v1 group is guarded by Auth: a request with no bearer token must be
// rejected at the boundary (401), never reaching a handler. This proves the
// group is protected without needing a valid Clerk token.
func TestNewRouter_V1_RequiresAuth(t *testing.T) {
	app := newRouter(newTestRouterDeps())

	req := httptest.NewRequest("GET", "/v1/ping", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test() error = %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 401 {
		t.Fatalf("GET /v1/ping without token status = %d, want 401", resp.StatusCode)
	}
}

// CORS preflight: the OPTIONS the browser sends before a cross-origin POST carries
// no token, so it must be answered by CORS (204 + echoed origin) BEFORE the /v1
// auth dispatch — otherwise it is 401'd and the browser blocks the real request.
func TestNewRouter_CORS_PreflightNotBlockedByAuth(t *testing.T) {
	app := newRouter(newTestRouterDeps())

	req := httptest.NewRequest("OPTIONS", "/v1/billing/checkout", nil)
	req.Header.Set("Origin", testCORSOrigin)
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "authorization,content-type")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test() error = %v", err)
	}
	defer resp.Body.Close()

	// fiber's cors short-circuits a valid preflight with 204 No Content.
	if resp.StatusCode != 204 {
		t.Fatalf("OPTIONS preflight status = %d, want 204 (not 401)", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != testCORSOrigin {
		t.Errorf("Access-Control-Allow-Origin = %q, want %q", got, testCORSOrigin)
	}
}

// The real cross-origin response must carry Access-Control-Allow-Origin so the
// browser exposes it to JS. The header is stamped before auth runs, so even a 401
// (no token) rides with it — which is what lets the FE read the api's answer at all.
func TestNewRouter_CORS_HeaderOnActualResponse(t *testing.T) {
	app := newRouter(newTestRouterDeps())

	req := httptest.NewRequest("GET", "/v1/identity/me", nil)
	req.Header.Set("Origin", testCORSOrigin)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test() error = %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != testCORSOrigin {
		t.Errorf("Access-Control-Allow-Origin = %q, want %q", got, testCORSOrigin)
	}
}

// The onboarding lookup subtree authenticates WITHOUT a tenant: with a resolver
// that would 401 any tenant route, a valid-token request to /v1/lookup must still
// reach the handler (200), while a tenant route (/v1/ping) with the same token is
// 401. This proves AuthUser is scoped to /v1/lookup and Auth guards the rest.
func TestNewRouter_Lookup_AuthenticatesWithoutTenant(t *testing.T) {
	deps := newTestRouterDeps()
	deps.resolver = erroringResolver{}
	deps.lookup = lookup.NewHandler(stubRegistry{})
	app := newRouter(deps)

	// Valid token, no resolvable tenant → lookup still succeeds.
	reqLookup := httptest.NewRequest("GET", "/v1/lookup/cnpj/19131243000197", nil)
	reqLookup.Header.Set("Authorization", "Bearer any.jwt.here")
	respLookup, err := app.Test(reqLookup)
	if err != nil {
		t.Fatalf("app.Test(lookup): %v", err)
	}
	defer respLookup.Body.Close()
	if respLookup.StatusCode != 200 {
		t.Fatalf("GET /v1/lookup/cnpj with token status = %d, want 200", respLookup.StatusCode)
	}

	// Same token on a tenant route → tenant Auth runs, resolver fails → 401.
	reqPing := httptest.NewRequest("GET", "/v1/ping", nil)
	reqPing.Header.Set("Authorization", "Bearer any.jwt.here")
	respPing, err := app.Test(reqPing)
	if err != nil {
		t.Fatalf("app.Test(ping): %v", err)
	}
	defer respPing.Body.Close()
	if respPing.StatusCode != 401 {
		t.Fatalf("GET /v1/ping with unresolvable tenant status = %d, want 401", respPing.StatusCode)
	}
}

// A lookup route with no bearer token must be rejected by AuthUser (401) before
// reaching the handler — the subtree is authenticated, just tenant-less.
func TestNewRouter_Lookup_RequiresToken(t *testing.T) {
	deps := newTestRouterDeps()
	deps.lookup = lookup.NewHandler(stubRegistry{})
	app := newRouter(deps)

	req := httptest.NewRequest("GET", "/v1/lookup/cnpj/19131243000197", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("GET /v1/lookup/cnpj without token status = %d, want 401", resp.StatusCode)
	}
}

// The Clerk webhook is not hand-listed in newRouter; identity mounts it via
// Register. Posting without svix headers must reach Handle and be rejected at the
// signature check (401) — proof the public route is wired through the composed
// router, not that any hand-written app.Post exists in main.
func TestNewRouter_Webhook_WiredThroughRegister(t *testing.T) {
	deps := newTestRouterDeps()
	deps.webhook = identity.NewWebhookHandler(testWebhookSecret, nil)
	app := newRouter(deps)

	req := httptest.NewRequest("POST", "/webhooks/clerk", strings.NewReader(`{}`))
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test() error = %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 401 {
		t.Fatalf("POST /webhooks/clerk without signature status = %d, want 401", resp.StatusCode)
	}
}

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/jusassessoria/platform/lib/httpx"
	"github.com/jusassessoria/platform/lib/telemetry"
)

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

func newTestRouterDeps() routerDeps {
	return routerDeps{
		logger:   telemetry.NewLogger(io.Discard, nil),
		verifier: fakeVerifier{},
		resolver: fakeResolver{},
		// webhook is never invoked by these tests; a nil handler is safe because
		// no route below reaches Handle. Left nil deliberately to keep the fixture
		// free of a real UseCase.
		webhook: nil,
	}
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

package draft

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/httpx"
	"github.com/jusassessoria/platform/lib/httpx/middleware"
)

// stubVerifier accepts any bearer token — Auth's job here is only to gate on the
// token's presence, not to test Clerk.
type stubVerifier struct{}

func (stubVerifier) Verify(context.Context, string) (userID, orgID, role string, err error) {
	return "clerk-user", "clerk-org", "", nil
}

// stubResolver returns a principal with the configured tenant, standing in for the
// identity slice's resolver.
type stubResolver struct{ tenant string }

func (r stubResolver) Resolve(context.Context, string, string) (httpx.Principal, error) {
	return httpx.Principal{UserID: "u-1", TenantID: r.tenant, Role: "LAWYER"}, nil
}

// panicIterator implements the iterator port and fails the test if Iterate is
// ever invoked — used by tests that must never reach the use case (e.g. a
// malformed body must fail at BodyParser, before Iterate is called).
type panicIterator struct{ t *testing.T }

func (p panicIterator) Iterate(context.Context, IterateCommand) (*IterateResult, error) {
	p.t.Fatal("Iterate must not be called")
	return nil, nil
}

// newAppWithIterator wires a Handler with the given iterator under the Auth
// boundary, mirroring production's /v1 group.
func newAppWithIterator(iter iterator, tenant string) *fiber.App {
	app := fiber.New(fiber.Config{
		ErrorHandler: func(c *fiber.Ctx, err error) error { return httpx.WriteError(c, err) },
	})
	v1 := app.Group("/v1", middleware.Auth(stubVerifier{}, stubResolver{tenant: tenant}))
	NewHandler(nil).WithIterator(iter).RegisterV1(v1)
	return app
}

// doJSON drives one request with a JSON body through app, returning status and raw body.
func doJSON(t *testing.T, app *fiber.App, method, path, bearer, body string) (int, string) {
	t.Helper()

	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	if bearer != "" {
		req.Header.Set(fiber.HeaderAuthorization, "Bearer "+bearer)
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, string(raw)
}

// TestHandler_IteratePeca_MalformedBody_400 pins the fix for a QA-found bug:
// a malformed body (scope as a string instead of an object) must fail as a
// client error (400 DOMAIN_ERROR_INVALID), not escape as a 500 INFRA_ERROR.
// The use case must never be reached.
func TestHandler_IteratePeca_MalformedBody_400(t *testing.T) {
	t.Parallel()

	app := newAppWithIterator(panicIterator{t: t}, "tenant-1")

	status, body := doJSON(t, app, http.MethodPost, "/v1/pecas/draft-1/iterate", "jwt", `{"scope":"whole"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", status, body)
	}

	var got httpx.ErrorBody
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("unmarshal error body: %v (body: %s)", err, body)
	}
	if got.Kind != string(apperr.KindInvalid) {
		t.Errorf("kind = %q, want %q", got.Kind, apperr.KindInvalid)
	}
}

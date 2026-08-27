package onboarding

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/httpx"
	"github.com/jusassessoria/platform/lib/httpx/middleware"
)

// --- HTTP test doubles -------------------------------------------------------

// stubVerifier accepts any bearer token and returns a fixed Clerk identity —
// the auth middleware's job in these tests is only to gate on the token's
// presence; the interesting scoping comes from stubResolver below.
type stubVerifier struct{}

func (stubVerifier) Verify(context.Context, string) (userID, orgID, role string, err error) {
	return "clerk-user", "clerk-org", "", nil
}

// stubResolver returns a fixed principal — the role/tenant/user a test wants
// the handler to see, standing in for the identity slice's real resolver.
type stubResolver struct {
	role   string
	tenant string
	user   string
}

func (r stubResolver) Resolve(context.Context, string, string) (httpx.Principal, error) {
	return httpx.Principal{UserID: r.user, TenantID: r.tenant, Role: r.role}, nil
}

// fakeHandlerUC records what the handler passed and returns canned results.
type fakeHandlerUC struct {
	progress          Progress
	progressErr       error
	gotProgressTenant string
	gotProgressUser   string

	dismissErr       error
	dismissCalled    bool
	gotDismissTenant string
	gotDismissUser   string
}

func (f *fakeHandlerUC) GetProgress(_ context.Context, tenantID, appUserID string) (Progress, error) {
	f.gotProgressTenant = tenantID
	f.gotProgressUser = appUserID
	return f.progress, f.progressErr
}

func (f *fakeHandlerUC) Dismiss(_ context.Context, tenantID, appUserID string) error {
	f.dismissCalled = true
	f.gotDismissTenant = tenantID
	f.gotDismissUser = appUserID
	return f.dismissErr
}

// newApp mounts onboarding's routes under the tenant-strict Auth with a
// principal of the given role/tenant/user, mirroring the production dispatch.
func newApp(uc handlerUC, role, tenant, user string) *fiber.App {
	app := fiber.New(fiber.Config{
		ErrorHandler: func(c *fiber.Ctx, err error) error { return httpx.WriteError(c, err) },
	})
	v1 := app.Group("/v1", middleware.Auth(stubVerifier{}, stubResolver{role: role, tenant: tenant, user: user}))
	NewHandler(uc).Register(v1)
	return app
}

// do drives one request through app, returning status and raw body.
func do(t *testing.T, app *fiber.App, method, path, body, bearer string) (int, string) {
	t.Helper()

	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
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

// --- GET /v1/onboarding/progress ---------------------------------------------

func TestHandler_GetProgress_200(t *testing.T) {
	t.Parallel()

	dismissed := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)
	uc := &fakeHandlerUC{progress: Progress{
		Steps:       Steps{SourcesConnected: true, FirstTriagem: true},
		DismissedAt: &dismissed,
	}}
	app := newApp(uc, "LAWYER", "tenant-1", "user-1")

	status, body := do(t, app, http.MethodGet, "/v1/onboarding/progress", "", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, body)
	}
	if uc.gotProgressTenant != "tenant-1" || uc.gotProgressUser != "user-1" {
		t.Fatalf("scope = (%q, %q), want (tenant-1, user-1)", uc.gotProgressTenant, uc.gotProgressUser)
	}
	for _, want := range []string{
		`"sources_connected":true`, `"members_invited":false`, `"first_triagem":true`,
		`"first_analise":false`, `"first_peca":false`, `"dismissed_at":"2026-08-27T10:00:00Z"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q: %s", want, body)
		}
	}
}

func TestHandler_GetProgress_NeverDismissed_NullDismissedAt(t *testing.T) {
	t.Parallel()

	uc := &fakeHandlerUC{progress: Progress{}}
	app := newApp(uc, "ADMIN", "tenant-1", "user-1")

	status, body := do(t, app, http.MethodGet, "/v1/onboarding/progress", "", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, body)
	}
	if !strings.Contains(body, `"dismissed_at":null`) {
		t.Fatalf("dismissed_at should be null before the first dismiss: %s", body)
	}
}

// TestHandler_GetProgress_AnyRole_NotGated proves the read is open to any
// authenticated role — no RequireRole guards it, per the architect's decision.
func TestHandler_GetProgress_AnyRole_NotGated(t *testing.T) {
	t.Parallel()

	for _, role := range []string{"ADMIN", "LAWYER"} {
		uc := &fakeHandlerUC{}
		app := newApp(uc, role, "tenant-9", "user-9")

		status, _ := do(t, app, http.MethodGet, "/v1/onboarding/progress", "", "jwt")
		if status != http.StatusOK {
			t.Fatalf("role %s: status = %d, want 200", role, status)
		}
	}
}

// TestHandler_GetProgress_ScopedFromPrincipal proves tenant_id and the
// caller's app_user id are read from the verified principal, never a query
// param or body — a caller cannot ask for another tenant's progress.
func TestHandler_GetProgress_ScopedFromPrincipal(t *testing.T) {
	t.Parallel()

	uc := &fakeHandlerUC{}
	app := newApp(uc, "LAWYER", "tenant-9", "user-9")

	if status, _ := do(t, app, http.MethodGet, "/v1/onboarding/progress?tenant_id=tenant-evil", "", "jwt"); status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if uc.gotProgressTenant != "tenant-9" || uc.gotProgressUser != "user-9" {
		t.Fatalf("scope = (%q, %q), want (tenant-9, user-9) from the principal, not the query string", uc.gotProgressTenant, uc.gotProgressUser)
	}
}

func TestHandler_GetProgress_NoPrincipal_401(t *testing.T) {
	t.Parallel()

	app := fiber.New(fiber.Config{
		ErrorHandler: func(c *fiber.Ctx, err error) error { return httpx.WriteError(c, err) },
	})
	NewHandler(&fakeHandlerUC{}).Register(app)

	status, _ := do(t, app, http.MethodGet, "/onboarding/progress", "", "")
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
}

func TestHandler_GetProgress_UseCaseError(t *testing.T) {
	t.Parallel()

	uc := &fakeHandlerUC{progressErr: apperr.NewInfra("boom", nil)}
	app := newApp(uc, "LAWYER", "tenant-1", "user-1")

	status, _ := do(t, app, http.MethodGet, "/v1/onboarding/progress", "", "jwt")
	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", status)
	}
}

// --- PATCH /v1/onboarding/dismiss --------------------------------------------

func TestHandler_Dismiss_204(t *testing.T) {
	t.Parallel()

	uc := &fakeHandlerUC{}
	app := newApp(uc, "LAWYER", "tenant-1", "user-1")

	status, body := do(t, app, http.MethodPatch, "/v1/onboarding/dismiss", "", "jwt")
	if status != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", status, body)
	}
	if !uc.dismissCalled {
		t.Fatal("Dismiss was not called")
	}
	if uc.gotDismissTenant != "tenant-1" || uc.gotDismissUser != "user-1" {
		t.Fatalf("scope = (%q, %q), want (tenant-1, user-1)", uc.gotDismissTenant, uc.gotDismissUser)
	}
}

// TestHandler_Dismiss_Idempotent proves calling dismiss twice never breaks —
// both calls answer 204, exactly what the upsert (ON CONFLICT DO UPDATE)
// guarantees at the SQL layer.
func TestHandler_Dismiss_Idempotent(t *testing.T) {
	t.Parallel()

	uc := &fakeHandlerUC{}
	app := newApp(uc, "LAWYER", "tenant-1", "user-1")

	for i := range 2 {
		status, body := do(t, app, http.MethodPatch, "/v1/onboarding/dismiss", "", "jwt")
		if status != http.StatusNoContent {
			t.Fatalf("call %d: status = %d, want 204; body=%s", i+1, status, body)
		}
	}
}

func TestHandler_Dismiss_AnyRole(t *testing.T) {
	t.Parallel()

	for _, role := range []string{"ADMIN", "LAWYER"} {
		uc := &fakeHandlerUC{}
		app := newApp(uc, role, "tenant-1", "user-1")

		status, _ := do(t, app, http.MethodPatch, "/v1/onboarding/dismiss", "", "jwt")
		if status != http.StatusNoContent {
			t.Fatalf("role %s: status = %d, want 204", role, status)
		}
	}
}

func TestHandler_Dismiss_UseCaseError(t *testing.T) {
	t.Parallel()

	uc := &fakeHandlerUC{dismissErr: apperr.NewInfra("boom", nil)}
	app := newApp(uc, "LAWYER", "tenant-1", "user-1")

	status, _ := do(t, app, http.MethodPatch, "/v1/onboarding/dismiss", "", "jwt")
	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", status)
	}
}

package portalcredential_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/jusassessoria/platform/internal/portalcredential"
	"github.com/jusassessoria/platform/lib/httpx"
	"github.com/jusassessoria/platform/lib/httpx/middleware"
)

// --- HTTP test doubles (same molde as internal/acquisition/handler_test.go) --

type stubVerifier struct{}

func (stubVerifier) Verify(context.Context, string) (userID, orgID, role string, err error) {
	return "clerk-user", "clerk-org", "", nil
}

type stubResolver struct {
	tenant string
	user   string
}

func (r stubResolver) Resolve(context.Context, string, string) (httpx.Principal, error) {
	return httpx.Principal{UserID: r.user, TenantID: r.tenant, Role: "LAWYER"}, nil
}

// fakeUC records what the handler forwarded and returns canned results/errors.
type fakeUC struct {
	configureResp *portalcredential.PortalCredential
	configureErr  error
	getResp       *portalcredential.PortalCredential
	getErr        error
	deleteErr     error

	gotTenant, gotUser, gotLogin, gotPassword string
}

func (f *fakeUC) Configure(_ context.Context, tenantID, appUserID, login, password string) (*portalcredential.PortalCredential, error) {
	f.gotTenant, f.gotUser, f.gotLogin, f.gotPassword = tenantID, appUserID, login, password
	return f.configureResp, f.configureErr
}

func (f *fakeUC) Get(_ context.Context, tenantID, appUserID string) (*portalcredential.PortalCredential, error) {
	f.gotTenant, f.gotUser = tenantID, appUserID
	return f.getResp, f.getErr
}

func (f *fakeUC) Delete(_ context.Context, tenantID, appUserID string) error {
	f.gotTenant, f.gotUser = tenantID, appUserID
	return f.deleteErr
}

func newApp(uc *fakeUC, tenant, user string) *fiber.App {
	app := fiber.New(fiber.Config{
		ErrorHandler: func(c *fiber.Ctx, err error) error { return httpx.WriteError(c, err) },
	})
	v1 := app.Group("/v1", middleware.Auth(stubVerifier{}, stubResolver{tenant: tenant, user: user}))
	portalcredential.NewHandler(uc).RegisterV1(v1)
	return app
}

func do(t *testing.T, app *fiber.App, method, path, body string) (int, string) {
	t.Helper()

	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set(fiber.HeaderAuthorization, "Bearer any-token")

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

// --- PUT (configure) -----------------------------------------------------------

func TestHandler_Configure_Success_200AndForwardsPrincipal(t *testing.T) {
	t.Parallel()

	uc := &fakeUC{configureResp: &portalcredential.PortalCredential{
		Login:  "advogado",
		Status: portalcredential.StatusActive,
	}}
	app := newApp(uc, "tenant-1", "user-1")

	status, body := do(t, app, fiber.MethodPut, "/v1/scraping/portal-credential", `{"login":"advogado","password":"senha-forte"}`)
	if status != fiber.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", status, body)
	}
	if uc.gotTenant != "tenant-1" || uc.gotUser != "user-1" {
		t.Errorf("forwarded (tenant,user) = (%q,%q), want (tenant-1,user-1)", uc.gotTenant, uc.gotUser)
	}
	if uc.gotLogin != "advogado" || uc.gotPassword != "senha-forte" {
		t.Errorf("forwarded (login,password) = (%q,%q), want (advogado,senha-forte)", uc.gotLogin, uc.gotPassword)
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if _, leaked := got["password"]; leaked {
		t.Error("response body leaks the password field")
	}
	if _, leaked := got["credential_ref"]; leaked {
		t.Error("response body leaks credential_ref")
	}
	if got["login"] != "advogado" {
		t.Errorf("login = %v, want advogado", got["login"])
	}
}

func TestHandler_Configure_Rejected_400(t *testing.T) {
	t.Parallel()

	uc := &fakeUC{configureErr: portalcredential.ErrPortalRejectedCredential}
	app := newApp(uc, "tenant-1", "user-1")

	status, _ := do(t, app, fiber.MethodPut, "/v1/scraping/portal-credential", `{"login":"advogado","password":"senha-errada"}`)
	if status != fiber.StatusBadRequest {
		t.Errorf("status = %d, want 400", status)
	}
}

func TestHandler_Configure_MissingFields_400(t *testing.T) {
	t.Parallel()

	uc := &fakeUC{}
	app := newApp(uc, "tenant-1", "user-1")

	status, _ := do(t, app, fiber.MethodPut, "/v1/scraping/portal-credential", `{"login":""}`)
	if status != fiber.StatusBadRequest {
		t.Errorf("status = %d, want 400", status)
	}
	if uc.gotLogin != "" {
		t.Error("use case was called despite an invalid body")
	}
}

func TestHandler_Configure_NoToken_401(t *testing.T) {
	t.Parallel()

	app := newApp(&fakeUC{}, "tenant-1", "user-1")

	req := httptest.NewRequest(fiber.MethodPut, "/v1/scraping/portal-credential", strings.NewReader(`{"login":"a","password":"b"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestHandler_Configure_NeverAcceptsTenantOrUserFromBody(t *testing.T) {
	t.Parallel()

	uc := &fakeUC{configureResp: &portalcredential.PortalCredential{Login: "advogado", Status: portalcredential.StatusActive}}
	app := newApp(uc, "tenant-from-token", "user-from-token")

	// A body that TRIES to smuggle tenant_id/app_user_id — the request struct has
	// no such fields, so BodyParser silently ignores them; the handler must still
	// only ever use the principal's values.
	body := `{"login":"advogado","password":"senha","tenant_id":"attacker-tenant","app_user_id":"attacker-user"}`
	status, _ := do(t, app, fiber.MethodPut, "/v1/scraping/portal-credential", body)
	if status != fiber.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if uc.gotTenant != "tenant-from-token" || uc.gotUser != "user-from-token" {
		t.Errorf("forwarded (tenant,user) = (%q,%q), want the principal's, not the body's", uc.gotTenant, uc.gotUser)
	}
}

// --- GET -----------------------------------------------------------------------

func TestHandler_Get_Found_200(t *testing.T) {
	t.Parallel()

	verified := time.Now().UTC()
	uc := &fakeUC{getResp: &portalcredential.PortalCredential{
		Login:          "advogado",
		Status:         portalcredential.StatusActive,
		LastVerifiedAt: verified,
	}}
	app := newApp(uc, "tenant-1", "user-1")

	status, body := do(t, app, fiber.MethodGet, "/v1/scraping/portal-credential", "")
	if status != fiber.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", status, body)
	}
	if uc.gotTenant != "tenant-1" || uc.gotUser != "user-1" {
		t.Errorf("forwarded (tenant,user) = (%q,%q), want (tenant-1,user-1)", uc.gotTenant, uc.gotUser)
	}
}

func TestHandler_Get_NotConfigured_404(t *testing.T) {
	t.Parallel()

	uc := &fakeUC{getErr: portalcredential.ErrPortalCredentialNotFound}
	app := newApp(uc, "tenant-1", "user-1")

	status, _ := do(t, app, fiber.MethodGet, "/v1/scraping/portal-credential", "")
	if status != fiber.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
}

// --- DELETE ----------------------------------------------------------------------

func TestHandler_Delete_Success_204(t *testing.T) {
	t.Parallel()

	uc := &fakeUC{}
	app := newApp(uc, "tenant-1", "user-1")

	status, body := do(t, app, fiber.MethodDelete, "/v1/scraping/portal-credential", "")
	if status != fiber.StatusNoContent {
		t.Fatalf("status = %d, want 204, body=%s", status, body)
	}
	if uc.gotTenant != "tenant-1" || uc.gotUser != "user-1" {
		t.Errorf("forwarded (tenant,user) = (%q,%q), want (tenant-1,user-1)", uc.gotTenant, uc.gotUser)
	}
}

func TestHandler_Delete_NotConfigured_404(t *testing.T) {
	t.Parallel()

	uc := &fakeUC{deleteErr: portalcredential.ErrPortalCredentialNotFound}
	app := newApp(uc, "tenant-1", "user-1")

	status, _ := do(t, app, fiber.MethodDelete, "/v1/scraping/portal-credential", "")
	if status != fiber.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
}

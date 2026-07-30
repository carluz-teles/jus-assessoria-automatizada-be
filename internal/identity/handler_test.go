package identity

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/jusassessoria/platform/lib/httpx"
	"github.com/jusassessoria/platform/lib/httpx/middleware"
)

// --- HTTP test doubles -------------------------------------------------------

// stubVerifier accepts any bearer token and returns a fixed Clerk identity — the
// auth middleware's job in these tests is only to gate on the token's presence.
type stubVerifier struct{}

func (stubVerifier) Verify(context.Context, string) (userID, orgID, role string, err error) {
	return "clerk-user", "clerk-org", "", nil
}

// stubResolver returns a principal with the configured role and tenant, standing
// in for the identity slice's own resolver on the tenant-strict routes.
type stubResolver struct {
	role   string
	tenant string
}

func (r stubResolver) Resolve(context.Context, string, string) (httpx.Principal, error) {
	return httpx.Principal{UserID: "u-1", TenantID: r.tenant, Role: r.role}, nil
}

// fakeHandlerUC records what the handler passed and returns canned results.
type fakeHandlerUC struct {
	me             Me
	meErr          error
	gotClerkUserID string

	profile     *Tenant
	gotTenantID string
	gotProfile  OrgProfile
	profileErr  error
}

func (f *fakeHandlerUC) GetMe(_ context.Context, clerkUserID string) (Me, error) {
	f.gotClerkUserID = clerkUserID
	return f.me, f.meErr
}

func (f *fakeHandlerUC) UpdateOrgProfile(_ context.Context, tenantID string, profile OrgProfile) (*Tenant, error) {
	f.gotTenantID = tenantID
	f.gotProfile = profile
	return f.profile, f.profileErr
}

// newMeApp mounts GET /identity/me under AuthUser (tenant-less), mirroring the
// production dispatch for the onboarding read.
func newMeApp(uc handlerUC) *fiber.App {
	app := fiber.New(fiber.Config{
		ErrorHandler: func(c *fiber.Ctx, err error) error { return httpx.WriteError(c, err) },
	})
	v1 := app.Group("/v1", middleware.AuthUser(stubVerifier{}))
	NewHandler(uc).RegisterMe(v1)
	return app
}

// newProfileApp mounts PUT /organization/profile under the tenant-strict Auth with
// a principal of the given role/tenant.
func newProfileApp(uc handlerUC, role, tenant string) *fiber.App {
	app := fiber.New(fiber.Config{
		ErrorHandler: func(c *fiber.Ctx, err error) error { return httpx.WriteError(c, err) },
	})
	v1 := app.Group("/v1", middleware.Auth(stubVerifier{}, stubResolver{role: role, tenant: tenant}))
	NewHandler(uc).RegisterV1(v1)
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

const validProfileBody = `{"cnpj":"12.345.678/0001-95","legal_name":"Escritório LTDA","trade_name":"Escritório","address":{"cep":"01311-902","logradouro":"Av Paulista","numero":"1000","cidade":"São Paulo","uf":"SP"}}`

// --- GET /identity/me --------------------------------------------------------

// AC1: an onboarded user → 200 with the internal tenant and gate; user_id echoes
// the Clerk id the AuthUser middleware injected.
func TestHandler_Me_WithTenant_200(t *testing.T) {
	t.Parallel()

	tenantID := "tenant-9"
	onboarded := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	uc := &fakeHandlerUC{me: Me{UserID: "clerk-user", TenantID: &tenantID, OnboardingCompletedAt: &onboarded}}
	app := newMeApp(uc)

	status, body := do(t, app, http.MethodGet, "/v1/identity/me", "", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, body)
	}
	if uc.gotClerkUserID != "clerk-user" {
		t.Fatalf("GetMe got clerk id %q, want the injected clerk-user", uc.gotClerkUserID)
	}
	if !strings.Contains(body, `"user_id":"clerk-user"`) || !strings.Contains(body, `"tenant_id":"tenant-9"`) {
		t.Fatalf("body missing user/tenant: %s", body)
	}
	if strings.Contains(body, `"tenant_id":null`) {
		t.Fatalf("tenant_id should not be null for an onboarded user: %s", body)
	}
}

// AC1: a signed-in user with no tenant yet → 200 with tenant_id and
// onboarding_completed_at as JSON null (not an error).
func TestHandler_Me_NoTenant_200Nulls(t *testing.T) {
	t.Parallel()

	uc := &fakeHandlerUC{me: Me{UserID: "clerk-user"}} // no tenant, no gate
	app := newMeApp(uc)

	status, body := do(t, app, http.MethodGet, "/v1/identity/me", "", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, body)
	}
	if !strings.Contains(body, `"tenant_id":null`) || !strings.Contains(body, `"onboarding_completed_at":null`) {
		t.Fatalf("want tenant_id/onboarding null, got: %s", body)
	}
	if !strings.Contains(body, `"user_id":"clerk-user"`) {
		t.Fatalf("user_id must still be present with no tenant: %s", body)
	}
}

// AC1: no bearer token → 401 at the AuthUser boundary; the handler never runs.
func TestHandler_Me_NoToken_401(t *testing.T) {
	t.Parallel()

	uc := &fakeHandlerUC{}
	app := newMeApp(uc)

	status, _ := do(t, app, http.MethodGet, "/v1/identity/me", "", "")
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
	if uc.gotClerkUserID != "" {
		t.Fatal("GetMe ran despite a missing token")
	}
}

// --- PUT /organization/profile ----------------------------------------------

// AC2: an ADMIN with a valid body → 200; tenant comes from the principal (not the
// body) and the CNPJ is persisted mask-free.
func TestHandler_UpdateProfile_Admin_200(t *testing.T) {
	t.Parallel()

	onboarded := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	uc := &fakeHandlerUC{profile: &Tenant{
		CNPJ:                  "12345678000195",
		LegalName:             "Escritório LTDA",
		TradeName:             "Escritório",
		Address:               &Address{CEP: "01311902", UF: "SP"},
		OnboardingCompletedAt: &onboarded,
	}}
	app := newProfileApp(uc, string(RoleAdmin), "tenant-42")

	status, body := do(t, app, http.MethodPut, "/v1/organization/profile", validProfileBody, "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, body)
	}
	if uc.gotTenantID != "tenant-42" {
		t.Fatalf("tenant passed to uc = %q, want tenant-42 (from principal)", uc.gotTenantID)
	}
	if uc.gotProfile.CNPJ != "12345678000195" {
		t.Fatalf("persisted cnpj = %q, want mask stripped", uc.gotProfile.CNPJ)
	}
	if !strings.Contains(body, `"cnpj":"12345678000195"`) {
		t.Fatalf("response missing saved cnpj: %s", body)
	}
}

// AC5: a non-ADMIN (LAWYER) → 403; the use case never runs.
func TestHandler_UpdateProfile_Lawyer_403(t *testing.T) {
	t.Parallel()

	uc := &fakeHandlerUC{}
	app := newProfileApp(uc, string(RoleLawyer), "tenant-1")

	status, _ := do(t, app, http.MethodPut, "/v1/organization/profile", validProfileBody, "jwt")
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
	if uc.gotTenantID != "" {
		t.Fatal("use case ran despite a 403")
	}
}

// AC5: no bearer token → 401 before the role guard.
func TestHandler_UpdateProfile_NoToken_401(t *testing.T) {
	t.Parallel()

	app := newProfileApp(&fakeHandlerUC{}, string(RoleAdmin), "tenant-1")

	status, _ := do(t, app, http.MethodPut, "/v1/organization/profile", validProfileBody, "")
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
}

// AC4: a validation failure is a 400 even for an ADMIN; the use case never runs.
func TestHandler_UpdateProfile_InvalidBody_400(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
	}{
		{name: "cnpj wrong length", body: `{"cnpj":"123","legal_name":"L","trade_name":"T","address":{"cep":"1","logradouro":"L","cidade":"C","uf":"SP"}}`},
		{name: "missing legal_name", body: `{"cnpj":"12345678000195","legal_name":"","trade_name":"T","address":{"cep":"1","logradouro":"L","cidade":"C","uf":"SP"}}`},
		{name: "missing address cep", body: `{"cnpj":"12345678000195","legal_name":"L","trade_name":"T","address":{"cep":"","logradouro":"L","cidade":"C","uf":"SP"}}`},
		{name: "malformed json", body: `{not json`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			uc := &fakeHandlerUC{}
			app := newProfileApp(uc, string(RoleAdmin), "tenant-1")

			status, body := do(t, app, http.MethodPut, "/v1/organization/profile", tt.body, "jwt")
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", status, body)
			}
			if uc.gotTenantID != "" {
				t.Fatal("use case ran on invalid input")
			}
		})
	}
}

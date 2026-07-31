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

// noOrgVerifier verifies the token but carries NO org — a signed-in user who
// belongs to no organization yet. AuthUser then leaves ClerkOrgFromCtx reporting
// absent, so the sync handler answers 401 (nothing to provision).
type noOrgVerifier struct{}

func (noOrgVerifier) Verify(context.Context, string) (userID, orgID, role string, err error) {
	return "clerk-user", "", "", nil
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

	syncMe         Me
	syncErr        error
	gotSyncOrgID   string
	gotSyncOrgName string
	gotSyncEmail   string
	gotSyncName    string
	gotSyncRole    Role

	profile     *Tenant
	gotTenantID string
	gotProfile  OrgProfile
	profileErr  error
}

func (f *fakeHandlerUC) GetMe(_ context.Context, clerkUserID string) (Me, error) {
	f.gotClerkUserID = clerkUserID
	return f.me, f.meErr
}

func (f *fakeHandlerUC) Sync(_ context.Context, clerkUserID, clerkOrgID, orgName, email, name string, role Role) (Me, error) {
	f.gotClerkUserID = clerkUserID
	f.gotSyncOrgID = clerkOrgID
	f.gotSyncOrgName = orgName
	f.gotSyncEmail = email
	f.gotSyncName = name
	f.gotSyncRole = role
	return f.syncMe, f.syncErr
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

// newSyncApp mounts the tenant-less onboarding routes (RegisterMe, which includes
// POST /identity/sync) under AuthUser with the given verifier, mirroring the
// production dispatch for the JIT provisioning endpoint.
func newSyncApp(uc handlerUC, v middleware.TokenVerifier) *fiber.App {
	app := fiber.New(fiber.Config{
		ErrorHandler: func(c *fiber.Ctx, err error) error { return httpx.WriteError(c, err) },
	})
	v1 := app.Group("/v1", middleware.AuthUser(v))
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

// --- POST /identity/sync -----------------------------------------------------

// AC5: a valid token + valid body provisions and returns 200 with the read model —
// tenant_id populated. Identity (user/org/role) comes from the token, never the body.
func TestHandler_Sync_Provisions_200(t *testing.T) {
	t.Parallel()

	tenantID := "tenant-jit"
	uc := &fakeHandlerUC{syncMe: Me{UserID: "clerk-user", TenantID: &tenantID}}
	app := newSyncApp(uc, stubVerifier{})

	body := `{"email":"ana@b.com","name":"Ana","org_name":"Escritório"}`
	status, respBody := do(t, app, http.MethodPost, "/v1/identity/sync", body, "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, respBody)
	}
	// Identity is taken from the token markers, never the body.
	if uc.gotClerkUserID != "clerk-user" || uc.gotSyncOrgID != "clerk-org" {
		t.Fatalf("uc got (user=%q, org=%q), want the token's clerk-user/clerk-org", uc.gotClerkUserID, uc.gotSyncOrgID)
	}
	// stubVerifier returns an empty org role → mapped to LAWYER (never silently admin).
	if uc.gotSyncRole != RoleLawyer {
		t.Fatalf("mapped role = %q, want LAWYER for an empty org role", uc.gotSyncRole)
	}
	if uc.gotSyncEmail != "ana@b.com" || uc.gotSyncName != "Ana" || uc.gotSyncOrgName != "Escritório" {
		t.Fatalf("display attrs = (%q,%q,%q), want the body values", uc.gotSyncEmail, uc.gotSyncName, uc.gotSyncOrgName)
	}
	if !strings.Contains(respBody, `"tenant_id":"tenant-jit"`) || strings.Contains(respBody, `"tenant_id":null`) {
		t.Fatalf("response must carry the provisioned tenant_id: %s", respBody)
	}
}

// AC5: no bearer token → 401 at the AuthUser boundary; the use case never runs.
func TestHandler_Sync_NoToken_401(t *testing.T) {
	t.Parallel()

	uc := &fakeHandlerUC{}
	app := newSyncApp(uc, stubVerifier{})

	status, _ := do(t, app, http.MethodPost, "/v1/identity/sync", `{"email":"a@b.com"}`, "")
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
	if uc.gotClerkUserID != "" {
		t.Fatal("Sync ran despite a missing token")
	}
}

// AC5: a verified token that carries NO org → 401 (nothing to provision), before the
// use case runs.
func TestHandler_Sync_NoOrgInToken_401(t *testing.T) {
	t.Parallel()

	uc := &fakeHandlerUC{}
	app := newSyncApp(uc, noOrgVerifier{})

	status, body := do(t, app, http.MethodPost, "/v1/identity/sync", `{"email":"a@b.com"}`, "jwt")
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", status, body)
	}
	if uc.gotSyncOrgID != "" {
		t.Fatal("Sync ran despite no org in the token")
	}
}

// AC5: an invalid body is a 400 in the {kind,message,details} envelope; the use case
// never runs.
func TestHandler_Sync_InvalidBody_400(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
	}{
		{name: "missing email", body: `{"name":"Ana"}`},
		{name: "malformed email", body: `{"email":"not-an-email"}`},
		{name: "malformed json", body: `{not json`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			uc := &fakeHandlerUC{}
			app := newSyncApp(uc, stubVerifier{})

			status, body := do(t, app, http.MethodPost, "/v1/identity/sync", tt.body, "jwt")
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", status, body)
			}
			if !strings.Contains(body, `"kind":`) {
				t.Fatalf("error must use the {kind,message,details} envelope: %s", body)
			}
			if uc.gotSyncEmail != "" || uc.gotSyncOrgID != "" {
				t.Fatal("Sync ran on an invalid body")
			}
		})
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

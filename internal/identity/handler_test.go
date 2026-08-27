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

	orgProfile         *Tenant
	gotReadTenantID    string
	orgProfileErr      error
	orgProfileReadFlag bool

	members          []OrgMember
	gotMembersTenant string
	membersErr       error

	removeErr         error
	removeCalled      bool
	gotRemoveTenantID string
	gotRemoveActorID  string
	gotRemoveTargetID string
}

func (f *fakeHandlerUC) GetMe(_ context.Context, clerkUserID string) (Me, error) {
	f.gotClerkUserID = clerkUserID
	return f.me, f.meErr
}

func (f *fakeHandlerUC) GetOrgProfile(_ context.Context, tenantID string) (*Tenant, error) {
	f.gotReadTenantID = tenantID
	f.orgProfileReadFlag = true
	return f.orgProfile, f.orgProfileErr
}

func (f *fakeHandlerUC) UpdateOrgProfile(_ context.Context, tenantID string, profile OrgProfile) (*Tenant, error) {
	f.gotTenantID = tenantID
	f.gotProfile = profile
	return f.profile, f.profileErr
}

func (f *fakeHandlerUC) ListOrgMembers(_ context.Context, tenantID string) ([]OrgMember, error) {
	f.gotMembersTenant = tenantID
	return f.members, f.membersErr
}

func (f *fakeHandlerUC) RemoveMember(_ context.Context, tenantID, actorUserID, targetAppUserID string) error {
	f.removeCalled = true
	f.gotRemoveTenantID = tenantID
	f.gotRemoveActorID = actorUserID
	f.gotRemoveTargetID = targetAppUserID
	return f.removeErr
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

// --- GET /organization/profile ----------------------------------------------

// AC1: an authenticated member → 200 with the full profile envelope
// (cnpj/legal/trade/phone/email/address/onboarding_completed_at); tenant comes from
// the principal, never the request.
func TestHandler_GetOrgProfile_200(t *testing.T) {
	t.Parallel()

	onboarded := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	uc := &fakeHandlerUC{orgProfile: &Tenant{
		CNPJ:                  "12345678000195",
		LegalName:             "Escritório LTDA",
		TradeName:             "Escritório",
		Address:               &Address{CEP: "01311902", Logradouro: "Av Paulista", Cidade: "São Paulo", UF: "SP"},
		Phone:                 "11987654321",
		Email:                 "contato@escritorio.com.br",
		OnboardingCompletedAt: &onboarded,
	}}
	app := newProfileApp(uc, string(RoleAdmin), "tenant-42")

	status, body := do(t, app, http.MethodGet, "/v1/organization/profile", "", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, body)
	}
	if uc.gotReadTenantID != "tenant-42" {
		t.Fatalf("tenant passed to uc = %q, want tenant-42 (from principal)", uc.gotReadTenantID)
	}
	for _, want := range []string{
		`"cnpj":"12345678000195"`,
		`"legal_name":"Escritório LTDA"`,
		`"trade_name":"Escritório"`,
		`"phone":"11987654321"`,
		`"email":"contato@escritorio.com.br"`,
		`"cep":"01311902"`,
		`"onboarding_completed_at":"2026-07-30T12:00:00Z"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("response missing %s: %s", want, body)
		}
	}
}

// AC3: the read is NOT admin-gated — a LAWYER opening the /organization page gets a
// 200, unlike the ADMIN-only write.
func TestHandler_GetOrgProfile_Lawyer_200(t *testing.T) {
	t.Parallel()

	uc := &fakeHandlerUC{orgProfile: &Tenant{CNPJ: "12345678000195", LegalName: "L", TradeName: "T"}}
	app := newProfileApp(uc, string(RoleLawyer), "tenant-42")

	status, body := do(t, app, http.MethodGet, "/v1/organization/profile", "", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a LAWYER read; body=%s", status, body)
	}
	if !uc.orgProfileReadFlag {
		t.Fatal("use case did not run for a LAWYER read")
	}
}

// AC2: a tenant that does not exist → ErrTenantNotFound mapped to 404 at the edge.
func TestHandler_GetOrgProfile_NotFound_404(t *testing.T) {
	t.Parallel()

	uc := &fakeHandlerUC{orgProfileErr: ErrTenantNotFound}
	app := newProfileApp(uc, string(RoleAdmin), "tenant-42")

	status, body := do(t, app, http.MethodGet, "/v1/organization/profile", "", "jwt")
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", status, body)
	}
}

// AC3: no bearer token → 401 at the Auth boundary; the handler never runs.
func TestHandler_GetOrgProfile_NoToken_401(t *testing.T) {
	t.Parallel()

	uc := &fakeHandlerUC{}
	app := newProfileApp(uc, string(RoleAdmin), "tenant-42")

	status, _ := do(t, app, http.MethodGet, "/v1/organization/profile", "", "")
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
	if uc.orgProfileReadFlag {
		t.Fatal("use case ran despite a missing token")
	}
}

// --- PUT /organization/profile ----------------------------------------------

// AC2: an ADMIN with a valid body → 200; tenant comes from the principal (not the
// body) and the CNPJ is persisted as typed (trimmed, no mask-stripping).
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
	if uc.gotProfile.CNPJ != "12.345.678/0001-95" {
		t.Fatalf("persisted cnpj = %q, want the trimmed value as typed", uc.gotProfile.CNPJ)
	}
	if !strings.Contains(body, `"cnpj":"12345678000195"`) {
		t.Fatalf("response missing saved cnpj: %s", body)
	}
}

// AC2: an optional phone in the body is validated, forwarded to the use case and
// echoed back in the profile view.
func TestHandler_UpdateProfile_EchoesPhone(t *testing.T) {
	t.Parallel()

	uc := &fakeHandlerUC{profile: &Tenant{
		CNPJ:      "12345678000195",
		LegalName: "Escritório LTDA",
		TradeName: "Escritório",
		Phone:     "11987654321",
	}}
	app := newProfileApp(uc, string(RoleAdmin), "tenant-42")

	body := `{"cnpj":"12.345.678/0001-95","legal_name":"Escritório LTDA","trade_name":"Escritório","phone":"11987654321","address":{"cep":"01311-902","logradouro":"Av Paulista","numero":"1000","cidade":"São Paulo","uf":"SP"}}`
	status, resp := do(t, app, http.MethodPut, "/v1/organization/profile", body, "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, resp)
	}
	if uc.gotProfile.Phone != "11987654321" {
		t.Fatalf("phone forwarded to uc = %q, want 11987654321", uc.gotProfile.Phone)
	}
	if !strings.Contains(resp, `"phone":"11987654321"`) {
		t.Fatalf("response missing echoed phone: %s", resp)
	}
}

// AC2: an optional email in the body is validated, forwarded to the use case and
// echoed back in the profile view.
func TestHandler_UpdateProfile_EchoesEmail(t *testing.T) {
	t.Parallel()

	uc := &fakeHandlerUC{profile: &Tenant{
		CNPJ:      "12345678000195",
		LegalName: "Escritório LTDA",
		TradeName: "Escritório",
		Email:     "contato@escritorio.com.br",
	}}
	app := newProfileApp(uc, string(RoleAdmin), "tenant-42")

	body := `{"cnpj":"12.345.678/0001-95","legal_name":"Escritório LTDA","trade_name":"Escritório","email":"contato@escritorio.com.br","address":{"cep":"01311-902","logradouro":"Av Paulista","numero":"1000","cidade":"São Paulo","uf":"SP"}}`
	status, resp := do(t, app, http.MethodPut, "/v1/organization/profile", body, "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, resp)
	}
	if uc.gotProfile.Email != "contato@escritorio.com.br" {
		t.Fatalf("email forwarded to uc = %q, want contato@escritorio.com.br", uc.gotProfile.Email)
	}
	if !strings.Contains(resp, `"email":"contato@escritorio.com.br"`) {
		t.Fatalf("response missing echoed email: %s", resp)
	}
}

// AC3: the address is optional as a whole — a body with NO address is a 200 and the
// use case runs (the profile is saved without an address).
func TestHandler_UpdateProfile_NoAddress_200(t *testing.T) {
	t.Parallel()

	uc := &fakeHandlerUC{profile: &Tenant{CNPJ: "12345678000195", LegalName: "L", TradeName: "T"}}
	app := newProfileApp(uc, string(RoleAdmin), "tenant-42")

	body := `{"cnpj":"12345678000195","legal_name":"L","trade_name":"T"}`
	status, resp := do(t, app, http.MethodPut, "/v1/organization/profile", body, "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, resp)
	}
	if uc.gotTenantID != "tenant-42" {
		t.Fatal("use case did not run for an address-less profile")
	}
	if uc.gotProfile.Address != (Address{}) {
		t.Fatalf("address forwarded = %+v, want the zero struct", uc.gotProfile.Address)
	}
}

// AC3: an address with a field filled but cidade/uf missing is a 400 (here cidade
// present but uf absent); the use case never runs. Street fields are optional, but
// cidade+uf are the required core.
func TestHandler_UpdateProfile_PartialAddress_400(t *testing.T) {
	t.Parallel()

	uc := &fakeHandlerUC{}
	app := newProfileApp(uc, string(RoleAdmin), "tenant-1")

	body := `{"cnpj":"12345678000195","legal_name":"L","trade_name":"T","address":{"cidade":"São Paulo"}}`
	status, resp := do(t, app, http.MethodPut, "/v1/organization/profile", body, "jwt")
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", status, resp)
	}
	if uc.gotTenantID != "" {
		t.Fatal("use case ran on a partial address")
	}
}

// AC2: a phone that is present but malformed (not 10–11 digits) is a 400; the use
// case never runs. An absent phone stays valid (covered by the Admin_200 case).
func TestHandler_UpdateProfile_InvalidPhone_400(t *testing.T) {
	t.Parallel()

	uc := &fakeHandlerUC{}
	app := newProfileApp(uc, string(RoleAdmin), "tenant-1")

	body := `{"cnpj":"12345678000195","legal_name":"L","trade_name":"T","phone":"119876543","address":{"cep":"1","logradouro":"L","cidade":"C","uf":"SP"}}`
	status, resp := do(t, app, http.MethodPut, "/v1/organization/profile", body, "jwt")
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", status, resp)
	}
	if uc.gotTenantID != "" {
		t.Fatal("use case ran on an invalid phone")
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
		{name: "missing cnpj", body: `{"cnpj":"","legal_name":"L","trade_name":"T","address":{"cep":"1","logradouro":"L","cidade":"C","uf":"SP"}}`},
		{name: "missing legal_name", body: `{"cnpj":"12345678000195","legal_name":"","trade_name":"T","address":{"cep":"1","logradouro":"L","cidade":"C","uf":"SP"}}`},
		{name: "address missing uf", body: `{"cnpj":"12345678000195","legal_name":"L","trade_name":"T","address":{"cidade":"C","uf":""}}`},
		{name: "malformed email", body: `{"cnpj":"12345678000195","legal_name":"L","trade_name":"T","email":"not-an-email","address":{"cep":"1","logradouro":"L","cidade":"C","uf":"SP"}}`},
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

// --- GET /organization/members -----------------------------------------------

// The members list is open to any authenticated member (a LAWYER opens the responsável
// selector too) → 200 with the {data:[...]} envelope; tenant comes from the principal.
func TestHandler_ListOrgMembers_200(t *testing.T) {
	t.Parallel()

	uc := &fakeHandlerUC{members: []OrgMember{
		{ID: "u-1", Name: "Dra. Ana", Email: "ana@e.com", Role: RoleAdmin},
		{ID: "u-2", Name: "Dr. Bruno", Email: "bruno@e.com", Role: RoleLawyer},
	}}
	app := newProfileApp(uc, string(RoleLawyer), "tenant-42")

	status, body := do(t, app, http.MethodGet, "/v1/organization/members", "", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, body)
	}
	if uc.gotMembersTenant != "tenant-42" {
		t.Errorf("tenant forwarded = %q, want tenant-42 (from principal)", uc.gotMembersTenant)
	}
	for _, want := range []string{`"data"`, `"id":"u-1"`, `"name":"Dra. Ana"`, `"email":"bruno@e.com"`, `"role":"LAWYER"`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %s\ngot: %s", want, body)
		}
	}
}

// An empty team serializes as data:[] (not null), so the FE selector renders an empty list.
func TestHandler_ListOrgMembers_Empty_200(t *testing.T) {
	t.Parallel()

	uc := &fakeHandlerUC{members: []OrgMember{}}
	app := newProfileApp(uc, string(RoleLawyer), "tenant-42")

	status, body := do(t, app, http.MethodGet, "/v1/organization/members", "", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, body)
	}
	if !strings.Contains(body, `"data":[]`) {
		t.Errorf("empty team should serialize as data:[], got: %s", body)
	}
}

// No bearer token → 401 at the Auth boundary; the handler never runs.
func TestHandler_ListOrgMembers_NoToken_401(t *testing.T) {
	t.Parallel()

	uc := &fakeHandlerUC{}
	app := newProfileApp(uc, string(RoleLawyer), "tenant-42")

	status, _ := do(t, app, http.MethodGet, "/v1/organization/members", "", "")
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
	if uc.gotMembersTenant != "" {
		t.Fatal("use case ran despite a missing token")
	}
}

// --- DELETE /v1/organization/members/:id -------------------------------------

// AC1/AC4: an ADMIN removing another member → 204; the use case runs with the
// principal's tenant/actor id and the path's target id.
func TestHandler_RemoveMember_Admin_204(t *testing.T) {
	t.Parallel()

	uc := &fakeHandlerUC{}
	app := newProfileApp(uc, string(RoleAdmin), "tenant-42")

	status, body := do(t, app, http.MethodDelete, "/v1/organization/members/target-uuid", "", "jwt")
	if status != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", status, body)
	}
	if !uc.removeCalled {
		t.Fatal("use case did not run")
	}
	if uc.gotRemoveTenantID != "tenant-42" || uc.gotRemoveActorID != "u-1" || uc.gotRemoveTargetID != "target-uuid" {
		t.Fatalf("RemoveMember args = (%q, %q, %q), want (tenant-42, u-1, target-uuid)",
			uc.gotRemoveTenantID, uc.gotRemoveActorID, uc.gotRemoveTargetID)
	}
}

// AC2: a non-ADMIN (LAWYER) → 403; the use case never runs.
func TestHandler_RemoveMember_Lawyer_403(t *testing.T) {
	t.Parallel()

	uc := &fakeHandlerUC{}
	app := newProfileApp(uc, string(RoleLawyer), "tenant-42")

	status, _ := do(t, app, http.MethodDelete, "/v1/organization/members/target-uuid", "", "jwt")
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
	if uc.removeCalled {
		t.Fatal("use case ran despite a 403")
	}
}

// AC3: the use case's self-removal guard surfaces as a typed 400, not a 500.
func TestHandler_RemoveMember_Self_400(t *testing.T) {
	t.Parallel()

	uc := &fakeHandlerUC{removeErr: ErrCannotRemoveSelf}
	app := newProfileApp(uc, string(RoleAdmin), "tenant-42")

	status, body := do(t, app, http.MethodDelete, "/v1/organization/members/u-1", "", "jwt")
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", status, body)
	}
}

// No bearer token → 401 at the Auth boundary; the handler never runs.
func TestHandler_RemoveMember_NoToken_401(t *testing.T) {
	t.Parallel()

	uc := &fakeHandlerUC{}
	app := newProfileApp(uc, string(RoleAdmin), "tenant-42")

	status, _ := do(t, app, http.MethodDelete, "/v1/organization/members/target-uuid", "", "")
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
	if uc.removeCalled {
		t.Fatal("use case ran despite a missing token")
	}
}

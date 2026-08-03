package acquisition

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/jusassessoria/platform/lib/httpx"
	"github.com/jusassessoria/platform/lib/httpx/middleware"
)

// --- HTTP test doubles -------------------------------------------------------

// stubVerifier accepts any bearer token — Auth's job here is only to gate on the
// token's presence, not to test Clerk.
type stubVerifier struct{}

func (stubVerifier) Verify(context.Context, string) (userID, orgID, role string, err error) {
	return "clerk-user", "clerk-org", "", nil
}

// stubResolver returns a principal with the configured role and tenant, standing
// in for the identity slice's resolver.
type stubResolver struct {
	role   string
	tenant string
}

func (r stubResolver) Resolve(context.Context, string, string) (httpx.Principal, error) {
	return httpx.Principal{UserID: "u-1", TenantID: r.tenant, Role: r.role}, nil
}

// fakeHandlerUC records what the handler passed and returns canned results.
type fakeHandlerUC struct {
	activateResp []*Integration
	listResp     []*Integration
	gotTenantID  string
	gotSources   []string
}

func (f *fakeHandlerUC) ActivateIntegration(_ context.Context, tenantID string, sources []string, _ Scope) ([]*Integration, error) {
	f.gotTenantID = tenantID
	f.gotSources = sources
	return f.activateResp, nil
}

func (f *fakeHandlerUC) ListIntegrations(_ context.Context, tenantID string) ([]*Integration, error) {
	f.gotTenantID = tenantID
	return f.listResp, nil
}

// fakeReader is a no-op read port for the write-path handler tests (the read
// routes have their own coverage).
type fakeReader struct{}

func (fakeReader) Processos(context.Context, ProcessosQuery) ([]ProcessoView, bool, error) {
	return nil, false, nil
}

func (fakeReader) Intimacoes(context.Context, IntimacoesQuery) ([]IntimacaoView, bool, error) {
	return nil, false, nil
}

// newApp builds an app whose /v1 group mirrors production: Auth resolves a
// principal with the given role/tenant, then the acquisition routes mount under
// it. An empty role/tenant still yields a valid principal (used by role tests).
func newApp(uc handlerUC, role, tenant string) *fiber.App {
	app := fiber.New(fiber.Config{
		ErrorHandler: func(c *fiber.Ctx, err error) error { return httpx.WriteError(c, err) },
	})
	v1 := app.Group("/v1", middleware.Auth(stubVerifier{}, stubResolver{role: role, tenant: tenant}))
	NewHandler(uc, fakeReader{}).RegisterV1(v1)
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

const validBody = `{"sources":["DJEN"],"scope":{"oab":["SP123456"]}}`

// --- tests -------------------------------------------------------------------

// AC6: no bearer token → 401 at the auth boundary, handler never runs.
func TestHandler_Activate_NoToken_401(t *testing.T) {
	t.Parallel()

	app := newApp(&fakeHandlerUC{}, roleAdmin, "tenant-1")
	status, _ := do(t, app, http.MethodPost, "/v1/acquisition/integrations", validBody, "")
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
}

// AC7: an authenticated LAWYER → 403 (activation is ADMIN-only).
func TestHandler_Activate_Lawyer_403(t *testing.T) {
	t.Parallel()

	app := newApp(&fakeHandlerUC{}, "LAWYER", "tenant-1")
	status, _ := do(t, app, http.MethodPost, "/v1/acquisition/integrations", validBody, "jwt")
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
}

// AC1 (handler): an ADMIN with a valid body → 201, tenant taken from the
// principal (not the body), and the response carries no credential_ref (AC10).
func TestHandler_Activate_Admin_201(t *testing.T) {
	t.Parallel()

	uc := &fakeHandlerUC{activateResp: []*Integration{
		{ID: "i1", Source: SourceDJEN, Scope: Scope{OAB: []string{"SP123456"}}, Status: StatusActive},
	}}
	app := newApp(uc, roleAdmin, "tenant-42")

	status, body := do(t, app, http.MethodPost, "/v1/acquisition/integrations", validBody, "jwt")
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", status, body)
	}
	if uc.gotTenantID != "tenant-42" {
		t.Fatalf("tenant passed to uc = %q, want tenant-42 (from principal)", uc.gotTenantID)
	}
	if len(uc.gotSources) != 1 || uc.gotSources[0] != SourceDJEN {
		t.Fatalf("sources passed = %v, want [DJEN]", uc.gotSources)
	}
	// AC10: credential_ref must never surface in the response.
	if strings.Contains(body, "credential_ref") {
		t.Fatalf("response leaked credential_ref: %s", body)
	}
}

// AC2/AC3/AC4 (via HTTP): a validation failure is a 400, even for an ADMIN. The
// use case is never called.
func TestHandler_Activate_InvalidBody_400(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
	}{
		{name: "AC4 empty sources", body: `{"sources":[],"scope":{"oab":["SP123456"]}}`},
		{name: "AC4 unsupported source", body: `{"sources":["UPLOAD"],"scope":{"oab":["SP123456"]}}`},
		{name: "AC2 empty oab", body: `{"sources":["DJEN"],"scope":{"oab":[]}}`},
		{name: "AC3 malformed oab", body: `{"sources":["DJEN"],"scope":{"oab":["bad"]}}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			uc := &fakeHandlerUC{}
			app := newApp(uc, roleAdmin, "tenant-1")
			status, body := do(t, app, http.MethodPost, "/v1/acquisition/integrations", tt.body, "jwt")
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", status, body)
			}
			if uc.gotSources != nil {
				t.Fatalf("use case was called on invalid input (sources=%v)", uc.gotSources)
			}
		})
	}
}

// AC9: GET returns the tenant's integrations, scoped by the principal's tenant.
func TestHandler_List_ScopedToTenant(t *testing.T) {
	t.Parallel()

	uc := &fakeHandlerUC{listResp: []*Integration{
		{ID: "i1", Source: SourceDJEN, Scope: Scope{OAB: []string{"SP1"}}, Status: StatusActive},
	}}
	app := newApp(uc, "LAWYER", "tenant-9") // read is open to any authenticated role

	status, body := do(t, app, http.MethodGet, "/v1/acquisition/integrations", "", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, body)
	}
	if uc.gotTenantID != "tenant-9" {
		t.Fatalf("tenant passed to uc = %q, want tenant-9", uc.gotTenantID)
	}
	if !strings.Contains(body, `"data"`) || !strings.Contains(body, SourceDJEN) {
		t.Fatalf("unexpected list body: %s", body)
	}
	if strings.Contains(body, "credential_ref") {
		t.Fatalf("response leaked credential_ref: %s", body)
	}
}

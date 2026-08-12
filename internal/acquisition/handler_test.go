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

func (fakeReader) Processos(context.Context, ProcessosQuery) (ProcessosResult, error) {
	return ProcessosResult{}, nil
}

func (fakeReader) Intimacoes(context.Context, IntimacoesQuery) (IntimacoesResult, error) {
	return IntimacoesResult{}, nil
}

func (fakeReader) ImportStatus(context.Context, string) (ImportStatusView, error) {
	return ImportStatusView{}, nil
}

func (fakeReader) Reconciliations(context.Context, string) (ReconciliationsView, error) {
	return ReconciliationsView{}, nil
}

func (fakeReader) ReconciliationDetail(context.Context, string, string) (ReconciliationDetailView, error) {
	return ReconciliationDetailView{}, nil
}

func (fakeReader) SyncRunItems(context.Context, string, string) (SyncRunItemsView, error) {
	return SyncRunItemsView{}, nil
}

// newApp builds an app whose /v1 group mirrors production: Auth resolves a
// principal with the given role/tenant, then the acquisition routes mount under
// it. An empty role/tenant still yields a valid principal (used by role tests).
func newApp(uc handlerUC, role, tenant string) *fiber.App {
	return newAppWithReader(uc, fakeReader{}, role, tenant)
}

// newAppWithReader is newApp with an explicit read port — the read-route tests wire a
// recording reader to assert what the handler forwards and returns.
func newAppWithReader(uc handlerUC, rd reader, role, tenant string) *fiber.App {
	app := fiber.New(fiber.Config{
		ErrorHandler: func(c *fiber.Ctx, err error) error { return httpx.WriteError(c, err) },
	})
	v1 := app.Group("/v1", middleware.Auth(stubVerifier{}, stubResolver{role: role, tenant: tenant}))
	NewHandler(uc, rd).RegisterV1(v1)
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

// --- read routes: /v1/processos search + pagination --------------------------

// recordingReader implements the handler's reader port, capturing the ProcessosQuery
// and returning a canned result — lets the HTTP tests assert query-param wiring and
// the envelope shape without a database.
type recordingReader struct {
	res      ProcessosResult
	gotQuery ProcessosQuery
}

func (r *recordingReader) Processos(_ context.Context, q ProcessosQuery) (ProcessosResult, error) {
	r.gotQuery = q
	return r.res, nil
}
func (r *recordingReader) Intimacoes(context.Context, IntimacoesQuery) (IntimacoesResult, error) {
	return IntimacoesResult{}, nil
}
func (r *recordingReader) ImportStatus(context.Context, string) (ImportStatusView, error) {
	return ImportStatusView{}, nil
}
func (r *recordingReader) Reconciliations(context.Context, string) (ReconciliationsView, error) {
	return ReconciliationsView{}, nil
}
func (r *recordingReader) ReconciliationDetail(context.Context, string, string) (ReconciliationDetailView, error) {
	return ReconciliationDetailView{}, nil
}
func (r *recordingReader) SyncRunItems(context.Context, string, string) (SyncRunItemsView, error) {
	return SyncRunItemsView{}, nil
}

// GET /v1/processos forwards ?search and the decoded ?cursor to the read port, and the
// tenant comes from the principal (never the query).
func TestHandler_ListProcessos_ForwardsSearchAndCursor(t *testing.T) {
	t.Parallel()

	rd := &recordingReader{}
	app := newAppWithReader(&fakeHandlerUC{}, rd, "LAWYER", "tenant-9")

	cursor := httpx.EncodeCursor(httpx.Cursor{
		LastSortValue: "0000123-45.2023.8.26.0001",
		LastID:        "018f0000-0000-7000-8000-000000000abc",
	})
	status, _ := do(t, app, http.MethodGet,
		"/v1/processos?search=petrobras&limit=25&cursor="+cursor, "", "jwt")

	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if rd.gotQuery.Search != "petrobras" {
		t.Errorf("Search = %q, want petrobras", rd.gotQuery.Search)
	}
	if rd.gotQuery.TenantID != "tenant-9" {
		t.Errorf("TenantID = %q, want tenant-9 (from principal)", rd.gotQuery.TenantID)
	}
	if rd.gotQuery.LastCNJ != "0000123-45.2023.8.26.0001" {
		t.Errorf("LastCNJ = %q, want the decoded cursor sort value", rd.gotQuery.LastCNJ)
	}
	if rd.gotQuery.Limit != 25 {
		t.Errorf("Limit = %d, want 25", rd.gotQuery.Limit)
	}
}

// ?limit above the ceiling is clamped to MaxLimit (100) — the handler never asks the
// repo for an unbounded page.
func TestHandler_ListProcessos_ClampsLimit(t *testing.T) {
	t.Parallel()

	rd := &recordingReader{}
	app := newAppWithReader(&fakeHandlerUC{}, rd, "LAWYER", "tenant-9")

	status, _ := do(t, app, http.MethodGet, "/v1/processos?limit=500", "", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if rd.gotQuery.Limit != 100 {
		t.Errorf("Limit = %d, want 100 (clamped)", rd.gotQuery.Limit)
	}
}

// A malformed ?cursor is a client error → 400, not a 500.
func TestHandler_ListProcessos_BadCursor_400(t *testing.T) {
	t.Parallel()

	app := newAppWithReader(&fakeHandlerUC{}, &recordingReader{}, "LAWYER", "tenant-9")
	status, body := do(t, app, http.MethodGet, "/v1/processos?cursor=not-a-cursor", "", "jwt")
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", status, body)
	}
}

// The response envelope carries the "X de Y" totals from the read result.
func TestHandler_ListProcessos_EnvelopeHasTotals(t *testing.T) {
	t.Parallel()

	rd := &recordingReader{res: ProcessosResult{
		Items:      []ProcessoView{{ID: "a", CNJNumber: "0001"}},
		TotalCount: 32,
		Total:      1247,
	}}
	app := newAppWithReader(&fakeHandlerUC{}, rd, "LAWYER", "tenant-9")

	status, body := do(t, app, http.MethodGet, "/v1/processos?search=petro", "", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	for _, want := range []string{`"total_count":32`, `"total":1247`} {
		if !strings.Contains(body, want) {
			t.Errorf("envelope missing %s\ngot: %s", want, body)
		}
	}
}

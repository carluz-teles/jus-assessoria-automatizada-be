package acquisition

import (
	"context"
	"encoding/json"
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
	activateResp   *Integration
	listResp       []*Integration
	gotTenantID    string
	gotScope       Scope
	activateCalled bool
	// Responsável write path (AssignResponsible): what the handler forwarded, and an
	// optional canned error to drive the failure branches.
	gotAssignTenant string
	gotAssignRecord string
	gotAssignUser   *string
	assignErr       error
	// Bulk responsável write path (BulkAssignResponsible): what the handler forwarded
	// (tenant, mode, filter, ids, user), a canned affected-count/error pair.
	gotBulkTenant string
	gotBulkAll    bool
	gotBulkQuery  ProcessosQuery
	gotBulkIDs    []string
	gotBulkUser   *string
	bulkResp      int64
	bulkErr       error
	// Triagem write path: what the handler forwarded (tenant, id) and which verb was
	// called, plus an optional canned error to drive the failure branch.
	gotTriageTenant string
	gotTriageID     string
	gotTriageVerb   string
	triageErr       error

	// watched_oab lifecycle (AddWatchedOAB/ToggleWatchedOAB): what the handler
	// forwarded, canned responses/errors to drive both branches.
	addWatchedOABResp    *WatchedOAB
	addWatchedOABErr     error
	gotAddWatchedOAB     OABEntry
	toggleWatchedOABResp *WatchedOAB
	toggleWatchedOABErr  error
	gotToggleOAB         string
	gotToggleEnabled     bool
}

func (f *fakeHandlerUC) ActivateIntegration(_ context.Context, tenantID string, scope Scope) (*Integration, error) {
	f.gotTenantID = tenantID
	f.gotScope = scope
	f.activateCalled = true
	return f.activateResp, nil
}

func (f *fakeHandlerUC) ListIntegrations(_ context.Context, tenantID string) ([]*Integration, error) {
	f.gotTenantID = tenantID
	return f.listResp, nil
}

func (f *fakeHandlerUC) AssignResponsible(_ context.Context, tenantID, courtRecordID string, assignedUserID *string) error {
	f.gotAssignTenant = tenantID
	f.gotAssignRecord = courtRecordID
	f.gotAssignUser = assignedUserID
	return f.assignErr
}

func (f *fakeHandlerUC) UpdateProcessoManual(_ context.Context, _, _ string, _ *string, _ *float64) error {
	return nil
}

func (f *fakeHandlerUC) BulkAssignResponsible(_ context.Context, tenantID string, all bool, q ProcessosQuery, ids []string, assignedUserID *string) (int64, error) {
	f.gotBulkTenant = tenantID
	f.gotBulkAll = all
	f.gotBulkQuery = q
	f.gotBulkIDs = ids
	f.gotBulkUser = assignedUserID
	return f.bulkResp, f.bulkErr
}

func (f *fakeHandlerUC) ResolveIntimacao(_ context.Context, tenantID, intimationID string) error {
	f.gotTriageTenant, f.gotTriageID, f.gotTriageVerb = tenantID, intimationID, "resolve"
	return f.triageErr
}

func (f *fakeHandlerUC) IgnoreIntimacao(_ context.Context, tenantID, intimationID string) error {
	f.gotTriageTenant, f.gotTriageID, f.gotTriageVerb = tenantID, intimationID, "ignore"
	return f.triageErr
}

func (f *fakeHandlerUC) ReopenIntimacao(_ context.Context, tenantID, intimationID string) error {
	f.gotTriageTenant, f.gotTriageID, f.gotTriageVerb = tenantID, intimationID, "reopen"
	return f.triageErr
}

func (f *fakeHandlerUC) AssignIntimacaoAssignee(_ context.Context, _, _ string, _ *string) error {
	return nil
}

func (f *fakeHandlerUC) BulkAssignIntimacoes(_ context.Context, _ string, _ bool, _ IntimacoesQuery, ids []string, _ *string) (int64, error) {
	return int64(len(ids)), nil
}

func (f *fakeHandlerUC) AddWatchedOAB(_ context.Context, _ string, oab OABEntry) (*WatchedOAB, error) {
	f.gotAddWatchedOAB = oab
	return f.addWatchedOABResp, f.addWatchedOABErr
}

func (f *fakeHandlerUC) ToggleWatchedOAB(_ context.Context, _, oab string, enabled bool) (*WatchedOAB, error) {
	f.gotToggleOAB = oab
	f.gotToggleEnabled = enabled
	return f.toggleWatchedOABResp, f.toggleWatchedOABErr
}

// fakeReader is a no-op read port for the write-path handler tests (the read
// routes have their own coverage).
type fakeReader struct{}

func (fakeReader) Processos(context.Context, ProcessosQuery) (ProcessosResult, error) {
	return ProcessosResult{}, nil
}

func (fakeReader) Processo(context.Context, string, string) (ProcessoView, error) {
	return ProcessoView{}, nil
}

func (fakeReader) Intimacoes(context.Context, IntimacoesQuery) (IntimacoesResult, error) {
	return IntimacoesResult{}, nil
}

func (fakeReader) Intimacao(context.Context, string, string) (IntimacaoDetailView, error) {
	return IntimacaoDetailView{}, nil
}

func (fakeReader) Andamentos(context.Context, AndamentosQuery) (AndamentosResult, error) {
	return AndamentosResult{}, nil
}

func (fakeReader) ActivityLog(context.Context, ActivityLogQuery) (ActivityLogResult, error) {
	return ActivityLogResult{}, nil
}

func (fakeReader) IntimacoesByProcesso(context.Context, IntimacoesByProcessoQuery) (IntimacoesByProcessoResult, error) {
	return IntimacoesByProcessoResult{}, nil
}

func (fakeReader) Partes(context.Context, string, string) (PartesView, error) {
	return PartesView{}, nil
}

func (fakeReader) ProcessosSummary(context.Context, string) (ProcessosSummaryView, error) {
	return ProcessosSummaryView{}, nil
}

func (fakeReader) IntimacoesSummary(context.Context, string) (IntimacoesSummaryView, error) {
	return IntimacoesSummaryView{}, nil
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

func (fakeReader) Captures(context.Context, string) (CapturesView, error) {
	return CapturesView{}, nil
}

func (fakeReader) CaptureDetail(context.Context, string, string) (CaptureRunView, error) {
	return CaptureRunView{}, nil
}

func (fakeReader) WatchedOABs(context.Context, string) ([]WatchedOABView, error) {
	return nil, nil
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
	NewHandler(uc, rd, nil, nil, nil, nil).RegisterV1(v1)
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

const validBody = `{"scope":{"oab":["SP123456"]}}`

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

	uc := &fakeHandlerUC{activateResp: &Integration{
		ID: "i1", Source: SourceDJEN, Scope: Scope{OAB: []string{"SP123456"}}, Status: StatusActive,
	}}
	app := newApp(uc, roleAdmin, "tenant-42")

	status, body := do(t, app, http.MethodPost, "/v1/acquisition/integrations", validBody, "jwt")
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", status, body)
	}
	if uc.gotTenantID != "tenant-42" {
		t.Fatalf("tenant passed to uc = %q, want tenant-42 (from principal)", uc.gotTenantID)
	}
	if len(uc.gotScope.OAB) != 1 || uc.gotScope.OAB[0] != "SP123456" {
		t.Fatalf("scope passed = %+v, want oab [SP123456]", uc.gotScope)
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
		{name: "AC2 empty oab", body: `{"scope":{"oab":[]}}`},
		{name: "AC3 malformed oab", body: `{"scope":{"oab":["bad"]}}`},
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
			if uc.activateCalled {
				t.Fatalf("use case was called on invalid input (scope=%+v)", uc.gotScope)
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

// --- oab-lookup route ---------------------------------------------------------

// fakeLawyerLookup is a controllable LawyerLookup double for the handler tests.
type fakeLawyerLookup struct {
	name      string
	err       error
	gotOAB    OABEntry
	wasCalled bool
}

func (f *fakeLawyerLookup) LookupOABName(_ context.Context, oab OABEntry) (string, error) {
	f.wasCalled = true
	f.gotOAB = oab
	return f.name, f.err
}

// newAppWithLawyers is newApp with an explicit LawyerLookup port wired.
func newAppWithLawyers(lawyers LawyerLookup, role, tenant string) *fiber.App {
	app := fiber.New(fiber.Config{
		ErrorHandler: func(c *fiber.Ctx, err error) error { return httpx.WriteError(c, err) },
	})
	v1 := app.Group("/v1", middleware.Auth(stubVerifier{}, stubResolver{role: role, tenant: tenant}))
	NewHandler(&fakeHandlerUC{}, fakeReader{}, nil, nil, nil, lawyers).RegisterV1(v1)
	return app
}

// GET /v1/acquisition/oab-lookup with a well-formed, matched OAB → 200 with the
// name. Any authenticated role may call it (no tenant/role gate, unlike activate).
func TestHandler_OABLookup_Found_200(t *testing.T) {
	t.Parallel()

	lawyers := &fakeLawyerLookup{name: "LUAN GOMES"}
	app := newAppWithLawyers(lawyers, "LAWYER", "tenant-1")

	status, body := do(t, app, http.MethodGet, "/v1/acquisition/oab-lookup?oab=SP347019", "", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, body)
	}
	if !strings.Contains(body, "LUAN GOMES") {
		t.Fatalf("body missing looked-up name: %s", body)
	}
	if !lawyers.wasCalled || lawyers.gotOAB != (OABEntry{UF: "SP", Number: "347019"}) {
		t.Fatalf("port called with %+v, want SP/347019", lawyers.gotOAB)
	}
}

// A malformed oab query param never reaches the port — 400 before any call.
func TestHandler_OABLookup_BadFormat_400(t *testing.T) {
	t.Parallel()

	lawyers := &fakeLawyerLookup{name: "unreachable"}
	app := newAppWithLawyers(lawyers, "LAWYER", "tenant-1")

	status, body := do(t, app, http.MethodGet, "/v1/acquisition/oab-lookup?oab=not-an-oab", "", "jwt")
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", status, body)
	}
	if lawyers.wasCalled {
		t.Fatal("port was called on invalid input")
	}
}

// No recent DJEN communication names the OAB → the typed ErrOABNotFound renders
// 404 — a normal, expected outcome for the wizard, not a fault.
func TestHandler_OABLookup_NotFound_404(t *testing.T) {
	t.Parallel()

	lawyers := &fakeLawyerLookup{err: ErrOABNotFound}
	app := newAppWithLawyers(lawyers, "LAWYER", "tenant-1")

	status, _ := do(t, app, http.MethodGet, "/v1/acquisition/oab-lookup?oab=SP347019", "", "jwt")
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", status)
	}
}

// No LawyerLookup wired (e.g. local dev without the DJEN connector configured) →
// a typed 503, same optional-port pattern as /resume with no resumer.
func TestHandler_OABLookup_NotWired_503(t *testing.T) {
	t.Parallel()

	app := newAppWithLawyers(nil, "LAWYER", "tenant-1")

	status, _ := do(t, app, http.MethodGet, "/v1/acquisition/oab-lookup?oab=SP347019", "", "jwt")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", status)
	}
}

// --- read routes: /v1/processos search + pagination --------------------------

// recordingReader implements the handler's reader port, capturing the ProcessosQuery
// and returning a canned result — lets the HTTP tests assert query-param wiring and
// the envelope shape without a database.
type recordingReader struct {
	res          ProcessosResult
	gotQuery     ProcessosQuery
	andRes       AndamentosResult
	gotAndQuery  AndamentosQuery
	actRes       ActivityLogResult
	gotActQuery  ActivityLogQuery
	intiRes      IntimacoesByProcessoResult
	gotIntiQuery IntimacoesByProcessoQuery
	// GET /v1/intimacoes — capture the forwarded query and return a canned result so
	// the top-level envelope (totals + filters options) can be asserted.
	intiListRes  IntimacoesResult
	gotIntiListQ IntimacoesQuery
	// GET /v1/intimacoes/:id — capture the forwarded (tenant, id) and return a canned
	// detail view or a typed error (a nil intiOneErr means the view is returned).
	intiOneRes    IntimacaoDetailView
	intiOneErr    error
	gotIntiOneTID string
	gotIntiOneID  string
	// Summary reads — canned views the handler wraps into the summary responses.
	procSummary ProcessosSummaryView
	intiSummary IntimacoesSummaryView
	// GET /v1/processos/:id — capture the forwarded (tenant, id) and return a canned
	// view or a typed error (a nil procOneErr means the view is returned).
	procOneRes    ProcessoView
	procOneErr    error
	gotProcOneTID string
	gotProcOneID  string
	// GET /v1/processos/:id/partes — capture the forwarded (tenant, court_record) and
	// return a canned view.
	partesRes     PartesView
	gotPartesTID  string
	gotPartesCRID string
}

func (r *recordingReader) Processos(_ context.Context, q ProcessosQuery) (ProcessosResult, error) {
	r.gotQuery = q
	return r.res, nil
}
func (r *recordingReader) Processo(_ context.Context, tenantID, id string) (ProcessoView, error) {
	r.gotProcOneTID, r.gotProcOneID = tenantID, id
	return r.procOneRes, r.procOneErr
}
func (r *recordingReader) Intimacoes(_ context.Context, q IntimacoesQuery) (IntimacoesResult, error) {
	r.gotIntiListQ = q
	return r.intiListRes, nil
}
func (r *recordingReader) Intimacao(_ context.Context, tenantID, id string) (IntimacaoDetailView, error) {
	r.gotIntiOneTID, r.gotIntiOneID = tenantID, id
	return r.intiOneRes, r.intiOneErr
}
func (r *recordingReader) Andamentos(_ context.Context, q AndamentosQuery) (AndamentosResult, error) {
	r.gotAndQuery = q
	return r.andRes, nil
}
func (r *recordingReader) ActivityLog(_ context.Context, q ActivityLogQuery) (ActivityLogResult, error) {
	r.gotActQuery = q
	return r.actRes, nil
}
func (r *recordingReader) IntimacoesByProcesso(_ context.Context, q IntimacoesByProcessoQuery) (IntimacoesByProcessoResult, error) {
	r.gotIntiQuery = q
	return r.intiRes, nil
}
func (r *recordingReader) Partes(_ context.Context, tenantID, courtRecordID string) (PartesView, error) {
	r.gotPartesTID, r.gotPartesCRID = tenantID, courtRecordID
	return r.partesRes, nil
}
func (r *recordingReader) ProcessosSummary(context.Context, string) (ProcessosSummaryView, error) {
	return r.procSummary, nil
}
func (r *recordingReader) IntimacoesSummary(context.Context, string) (IntimacoesSummaryView, error) {
	return r.intiSummary, nil
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
func (r *recordingReader) Captures(context.Context, string) (CapturesView, error) {
	return CapturesView{}, nil
}
func (r *recordingReader) CaptureDetail(context.Context, string, string) (CaptureRunView, error) {
	return CaptureRunView{}, nil
}
func (r *recordingReader) WatchedOABs(context.Context, string) ([]WatchedOABView, error) {
	return nil, nil
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

// The envelope's filters block is always present and never null: a zero/empty result
// must serialize filters as {} (the FE renders an empty chip row, not a blank block).
func TestHandler_ListProcessos_EnvelopeFiltersAlwaysObject(t *testing.T) {
	t.Parallel()

	rd := &recordingReader{res: ProcessosResult{}}
	app := newAppWithReader(&fakeHandlerUC{}, rd, "LAWYER", "tenant-9")

	status, body := do(t, app, http.MethodGet, "/v1/processos", "", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.Contains(body, `"filters":{}`) {
		t.Errorf("envelope filters not an empty object\ngot: %s", body)
	}
}

// GET /v1/intimacoes: the top-level envelope must carry the filters block as a
// non-null object with the type/user_status/court options the use case assembled —
// the regression guard for newIntimacoesPage serializing filters as null.
func TestHandler_ListIntimacoes_EnvelopeFiltersWithOptions(t *testing.T) {
	t.Parallel()

	f := httpx.Filters{}
	f.SetEnum("type", IntimationTypeIntimacao, IntimationTypeCitacao)
	f.SetEnum("user_status", IntimationUserStatusPending, IntimationUserStatusResolved)
	f.Set("court", httpx.OptionsFromStrings([]string{"TJSP"})...)
	rd := &recordingReader{intiListRes: IntimacoesResult{Filters: f}}
	app := newAppWithReader(&fakeHandlerUC{}, rd, "LAWYER", "tenant-9")

	status, body := do(t, app, http.MethodGet, "/v1/intimacoes", "", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, body)
	}
	var env httpx.Page[IntimacaoView]
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decode envelope: %v; body=%s", err, body)
	}
	if env.Filters == nil {
		t.Fatal("filters is null, want an object with the type/user_status/court options")
	}
	if got := len(env.Filters["type"]); got != 2 {
		t.Errorf("filters.type = %d options, want 2", got)
	} else if env.Filters["type"][0].Value != IntimationTypeIntimacao || env.Filters["type"][0].Label != IntimationTypeIntimacao {
		t.Errorf("filters.type[0] = %+v, want label==value %q", env.Filters["type"][0], IntimationTypeIntimacao)
	}
	if got := len(env.Filters["user_status"]); got != 2 {
		t.Errorf("filters.user_status = %d options, want 2", got)
	}
	if got := len(env.Filters["court"]); got != 1 || env.Filters["court"][0].Value != "TJSP" {
		t.Errorf("filters.court = %+v, want the TJSP option", env.Filters["court"])
	}
}

// ?urgencia is a closed set: valid values are forwarded to the read port.
func TestHandler_ListIntimacoes_ValidUrgencia_ForwardedToReader(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		urgencia string
	}{
		{name: "atraso", urgencia: UrgenciaAtraso},
		{name: "hoje", urgencia: UrgenciaHoje},
		{name: "proximos_dois_dias", urgencia: UrgenciaProximosDoisDias},
		{name: "semana", urgencia: UrgenciaSemana},
		{name: "este_mes", urgencia: UrgenciaEsteMes},
		{name: "mais_adiante", urgencia: UrgenciaMaisAdiante},
		{name: "sem_data_definida", urgencia: UrgenciaSemDataDefinida},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rd := &recordingReader{}
			app := newAppWithReader(&fakeHandlerUC{}, rd, "LAWYER", "tenant-9")

			status, body := do(t, app, http.MethodGet,
				"/v1/intimacoes?urgencia="+tt.urgencia, "", "jwt")
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", status, body)
			}
			if rd.gotIntiListQ.Urgencia != tt.urgencia {
				t.Errorf("forwarded Urgencia = %q, want %q", rd.gotIntiListQ.Urgencia, tt.urgencia)
			}
		})
	}
}

// ?urgencia=sem_providencia is a deprecated/removed bucket: it must NOT 400 (legacy
// deep-links keep working) and is demoted to "no filter" before reaching the read port.
func TestHandler_ListIntimacoes_SemProvidenciaDemotedToNoFilter(t *testing.T) {
	t.Parallel()

	rd := &recordingReader{}
	app := newAppWithReader(&fakeHandlerUC{}, rd, "LAWYER", "tenant-9")

	status, body := do(t, app, http.MethodGet,
		"/v1/intimacoes?urgencia="+UrgenciaSemProvidencia, "", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, body)
	}
	if rd.gotIntiListQ.Urgencia != "" {
		t.Errorf("forwarded Urgencia = %q, want empty (demoted from sem_providencia)", rd.gotIntiListQ.Urgencia)
	}
}

// ?nao_confirmado is the server-side "Não confirmadas" triage toggle: it forwards as a
// separate boolean filter on the query (combines with any temporal tab), independent of
// ?urgencia. Accepted values are "true"/"1".
func TestHandler_ListIntimacoes_NaoConfirmadoForwarded(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		param string
		want  bool
	}{
		{name: "true", param: "true", want: true},
		{name: "1", param: "1", want: true},
		{name: "absent", param: "", want: false},
		{name: "false", param: "false", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rd := &recordingReader{}
			app := newAppWithReader(&fakeHandlerUC{}, rd, "LAWYER", "tenant-9")

			path := "/v1/intimacoes?urgencia=" + UrgenciaAtraso
			if tt.param != "" {
				path += "&nao_confirmado=" + tt.param
			}
			status, body := do(t, app, http.MethodGet, path, "", "jwt")
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", status, body)
			}
			if rd.gotIntiListQ.NaoConfirmado != tt.want {
				t.Errorf("forwarded NaoConfirmado = %v, want %v", rd.gotIntiListQ.NaoConfirmado, tt.want)
			}
		})
	}
}

// ?urgencia with a value outside the closed set is a client error → 400.
func TestHandler_ListIntimacoes_InvalidUrgencia_400(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		urgencia string
	}{
		{name: "typo", urgencia: "urgente"},
		{name: "uppercase", urgencia: "ATRASO"},
		{name: "arbitrary", urgencia: "this_week"},
		{name: "nao_confirmado_is_now_a_chip_not_urgencia", urgencia: UrgenciaNaoConfirmado},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			app := newAppWithReader(&fakeHandlerUC{}, &recordingReader{}, "LAWYER", "tenant-9")
			status, _ := do(t, app, http.MethodGet,
				"/v1/intimacoes?urgencia="+tt.urgencia, "", "jwt")
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 for unknown urgencia %q", status, tt.urgencia)
			}
		})
	}
}

// The envelope's filters block includes the urgencia options so the FE can render
// the chip set without hard-coding the closed set.
func TestHandler_ListIntimacoes_EnvelopeFiltersIncludesUrgencia(t *testing.T) {
	t.Parallel()

	f := httpx.Filters{}
	f.SetEnum("urgencia", UrgenciaAtraso, UrgenciaHoje, UrgenciaProximosDoisDias, UrgenciaSemana, UrgenciaEsteMes, UrgenciaMaisAdiante, UrgenciaSemDataDefinida)
	rd := &recordingReader{intiListRes: IntimacoesResult{Filters: f}}
	app := newAppWithReader(&fakeHandlerUC{}, rd, "LAWYER", "tenant-9")

	status, body := do(t, app, http.MethodGet, "/v1/intimacoes", "", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, body)
	}
	var env intimacoesEnvelope
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decode envelope: %v; body=%s", err, body)
	}
	opts, ok := env.Filters["urgencia"]
	if !ok || len(opts) != 7 {
		t.Fatalf("filters.urgencia = %v (len=%d), want 7 options", opts, len(opts))
	}
	// Verify the canonical values are all present in order.
	want := []string{UrgenciaAtraso, UrgenciaHoje, UrgenciaProximosDoisDias, UrgenciaSemana, UrgenciaEsteMes, UrgenciaMaisAdiante, UrgenciaSemDataDefinida}
	for i, w := range want {
		if opts[i].Value != w {
			t.Errorf("filters.urgencia[%d].value = %q, want %q", i, opts[i].Value, w)
		}
	}
}

// ?assignee=me resolves to the authenticated principal's own user id (stubResolver
// always yields "u-1") and is forwarded to the read port — the "Minhas" toggle.
func TestHandler_ListIntimacoes_AssigneeMe_ResolvesToPrincipal(t *testing.T) {
	t.Parallel()

	rd := &recordingReader{}
	app := newAppWithReader(&fakeHandlerUC{}, rd, "LAWYER", "tenant-9")

	status, body := do(t, app, http.MethodGet, "/v1/intimacoes?assignee=me", "", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, body)
	}
	if rd.gotIntiListQ.Assignee != "u-1" {
		t.Errorf("forwarded Assignee = %q, want the principal's own id %q", rd.gotIntiListQ.Assignee, "u-1")
	}
}

// ?assignee=<uuid> is forwarded verbatim (no "me" resolution) to the read port.
func TestHandler_ListIntimacoes_AssigneeUUID_ForwardedVerbatim(t *testing.T) {
	t.Parallel()

	rd := &recordingReader{}
	app := newAppWithReader(&fakeHandlerUC{}, rd, "LAWYER", "tenant-9")

	const uid = "018f0000-0000-7000-8000-000000000abc"
	status, body := do(t, app, http.MethodGet, "/v1/intimacoes?assignee="+uid, "", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, body)
	}
	if rd.gotIntiListQ.Assignee != uid {
		t.Errorf("forwarded Assignee = %q, want %q", rd.gotIntiListQ.Assignee, uid)
	}
}

// ?assignee with a value that is neither "" nor "me" nor a well-formed uuid is a
// client error → 400.
func TestHandler_ListIntimacoes_InvalidAssignee_400(t *testing.T) {
	t.Parallel()

	app := newAppWithReader(&fakeHandlerUC{}, &recordingReader{}, "LAWYER", "tenant-9")
	status, _ := do(t, app, http.MethodGet, "/v1/intimacoes?assignee=not-a-uuid", "", "jwt")
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a malformed assignee", status)
	}
}

// ?court/?lifecycle/?degree/?assignee flow into the ProcessosQuery the handler sends
// to the read port — the same param set the envelope exposes as chips.
func TestHandler_ListProcessos_ForwardsFilters(t *testing.T) {
	t.Parallel()

	rd := &recordingReader{}
	app := newAppWithReader(&fakeHandlerUC{}, rd, "LAWYER", "tenant-9")

	status, _ := do(t, app, http.MethodGet,
		"/v1/processos?court=TJSP&lifecycle=SUSPENDED&degree=PRIMEIRO_GRAU&assignee=018f0000-0000-7000-8000-000000000abc",
		"", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if rd.gotQuery.Court != "TJSP" || rd.gotQuery.Degree != "PRIMEIRO_GRAU" {
		t.Errorf("free-text filters = (%q, %q), want (TJSP, PRIMEIRO_GRAU)", rd.gotQuery.Court, rd.gotQuery.Degree)
	}
	if rd.gotQuery.Lifecycle != "SUSPENDED" {
		t.Errorf("Lifecycle = %q, want SUSPENDED", rd.gotQuery.Lifecycle)
	}
	if rd.gotQuery.Assignee != "018f0000-0000-7000-8000-000000000abc" {
		t.Errorf("Assignee = %q, want the forwarded id", rd.gotQuery.Assignee)
	}
}

// A param outside the route's allowlist is a client error → 400, never silently
// ignored (docs/erd-backend.md §4e.3).
func TestHandler_ListProcessos_UnknownParam_400(t *testing.T) {
	t.Parallel()

	app := newAppWithReader(&fakeHandlerUC{}, &recordingReader{}, "LAWYER", "tenant-9")
	status, _ := do(t, app, http.MethodGet, "/v1/processos?type=CITACAO", "", "jwt")
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a foreign param", status)
	}
}

// ?lifecycle is a closed set: a value outside it (a typo, or a lifecycle that was
// removed) is a client error → 400, not a silently-empty page.
func TestHandler_ListProcessos_InvalidLifecycle_400(t *testing.T) {
	t.Parallel()

	app := newAppWithReader(&fakeHandlerUC{}, &recordingReader{}, "LAWYER", "tenant-9")
	status, _ := do(t, app, http.MethodGet, "/v1/processos?lifecycle=PAUSED", "", "jwt")
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an unknown lifecycle", status)
	}
}

// ?assignee must be a well-formed user id — a garbage value is a client error → 400.
func TestHandler_ListProcessos_InvalidAssignee_400(t *testing.T) {
	t.Parallel()

	app := newAppWithReader(&fakeHandlerUC{}, &recordingReader{}, "LAWYER", "tenant-9")
	status, _ := do(t, app, http.MethodGet, "/v1/processos?assignee=not-a-uuid", "", "jwt")
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a malformed assignee", status)
	}
}

// --- read route: /v1/processos/:id/partes -----------------------------------

// GET /v1/processos/:id/partes forwards the path :id (court_record id) and the tenant
// (from the principal, never the query) to the read port, and returns the bucketed view.
func TestHandler_ListPartes_ForwardsAndReturnsBuckets(t *testing.T) {
	t.Parallel()

	rd := &recordingReader{partesRes: PartesView{
		Autor: []PartyView{{Name: "AUTOR", Counsels: []PartyCounselView{{Name: "ADV", OAB: "111", UF: "SP"}}}},
		Reu:   []PartyView{{Name: "REU", Counsels: []PartyCounselView{}}},
	}}
	app := newAppWithReader(&fakeHandlerUC{}, rd, "LAWYER", "tenant-9")

	status, body := do(t, app, http.MethodGet, "/v1/processos/cr-42/partes", "", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", status, body)
	}
	if rd.gotPartesCRID != "cr-42" {
		t.Errorf("court_record = %q, want cr-42 (from path)", rd.gotPartesCRID)
	}
	if rd.gotPartesTID != "tenant-9" {
		t.Errorf("tenant = %q, want tenant-9 (from principal)", rd.gotPartesTID)
	}
	for _, want := range []string{`"autor"`, `"reu"`, `"terceiros"`, `"AUTOR"`, `"oab":"111"`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %s\ngot: %s", want, body)
		}
	}
}

// --- read route: /v1/processos/:id/andamentos -------------------------------

// GET /v1/processos/:id/andamentos forwards the path :id (the court_record id) and the
// decoded ?cursor to the read port, clamps ?limit, and takes the tenant from the
// principal (never the query).
func TestHandler_ListAndamentos_ForwardsProcessoAndCursor(t *testing.T) {
	t.Parallel()

	rd := &recordingReader{}
	app := newAppWithReader(&fakeHandlerUC{}, rd, "LAWYER", "tenant-9")

	cursor := httpx.EncodeCursor(httpx.Cursor{
		LastSortValue: "2024-03-01T12:30:00Z",
		LastID:        "018f0000-0000-7000-8000-000000000abc",
	})
	status, _ := do(t, app, http.MethodGet,
		"/v1/processos/cr-77/andamentos?limit=25&cursor="+cursor, "", "jwt")

	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if rd.gotAndQuery.CourtRecordID != "cr-77" {
		t.Errorf("CourtRecordID = %q, want cr-77 (from path)", rd.gotAndQuery.CourtRecordID)
	}
	if rd.gotAndQuery.TenantID != "tenant-9" {
		t.Errorf("TenantID = %q, want tenant-9 (from principal)", rd.gotAndQuery.TenantID)
	}
	if rd.gotAndQuery.LastOccurred != "2024-03-01T12:30:00Z" {
		t.Errorf("LastOccurred = %q, want the decoded cursor sort value", rd.gotAndQuery.LastOccurred)
	}
	if rd.gotAndQuery.LastID != "018f0000-0000-7000-8000-000000000abc" {
		t.Errorf("LastID = %q, want the decoded cursor id", rd.gotAndQuery.LastID)
	}
	if rd.gotAndQuery.Limit != 25 {
		t.Errorf("Limit = %d, want 25", rd.gotAndQuery.Limit)
	}
}

// The first page passes the max sentinel cursor (no ?cursor), and ?limit defaults to
// DefaultLimit when absent — the handler never asks the repo for an unbounded page.
func TestHandler_ListAndamentos_FirstPageSentinelAndDefaultLimit(t *testing.T) {
	t.Parallel()

	rd := &recordingReader{}
	app := newAppWithReader(&fakeHandlerUC{}, rd, "LAWYER", "tenant-9")

	status, _ := do(t, app, http.MethodGet, "/v1/processos/cr-1/andamentos", "", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if rd.gotAndQuery.Limit != httpx.DefaultLimit {
		t.Errorf("Limit = %d, want %d (default)", rd.gotAndQuery.Limit, httpx.DefaultLimit)
	}
	if rd.gotAndQuery.LastOccurred != maxTimestamp || rd.gotAndQuery.LastID != maxUUID {
		t.Errorf("first-page sentinel = (%q, %q), want (%q, %q)",
			rd.gotAndQuery.LastOccurred, rd.gotAndQuery.LastID, maxTimestamp, maxUUID)
	}
}

// ?limit above the ceiling is clamped to MaxLimit (100).
func TestHandler_ListAndamentos_ClampsLimit(t *testing.T) {
	t.Parallel()

	rd := &recordingReader{}
	app := newAppWithReader(&fakeHandlerUC{}, rd, "LAWYER", "tenant-9")

	status, _ := do(t, app, http.MethodGet, "/v1/processos/cr-1/andamentos?limit=500", "", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if rd.gotAndQuery.Limit != httpx.MaxLimit {
		t.Errorf("Limit = %d, want %d (clamped)", rd.gotAndQuery.Limit, httpx.MaxLimit)
	}
}

// A process with no andamentos serializes as an empty data array (never null) with the
// zero totals — 200, not 404.
func TestHandler_ListAndamentos_Empty(t *testing.T) {
	t.Parallel()

	rd := &recordingReader{andRes: AndamentosResult{}}
	app := newAppWithReader(&fakeHandlerUC{}, rd, "LAWYER", "tenant-9")

	status, body := do(t, app, http.MethodGet, "/v1/processos/cr-empty/andamentos", "", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	for _, want := range []string{`"data":[]`, `"next_cursor":null`, `"total":0`} {
		if !strings.Contains(body, want) {
			t.Errorf("envelope missing %s\ngot: %s", want, body)
		}
	}
}

// A malformed ?cursor is a client error → 400, not a 500.
func TestHandler_ListAndamentos_BadCursor_400(t *testing.T) {
	t.Parallel()

	app := newAppWithReader(&fakeHandlerUC{}, &recordingReader{}, "LAWYER", "tenant-9")
	status, body := do(t, app, http.MethodGet, "/v1/processos/cr-1/andamentos?cursor=not-a-cursor", "", "jwt")
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", status, body)
	}
}

// Cursor round-trip across two pages: when the read reports a further page, the
// envelope carries a next_cursor keyed off the last row's (occurred_at, id); echoing
// it back resumes the keyset exactly there.
func TestHandler_ListAndamentos_CursorRoundTrip(t *testing.T) {
	t.Parallel()

	last := AndamentoView{
		ID:         "018f0000-0000-7000-8000-0000000000ff",
		OccurredAt: time.Date(2024, 3, 1, 12, 30, 0, 0, time.UTC),
	}
	rd := &recordingReader{andRes: AndamentosResult{
		Items:   []AndamentoView{{ID: "first"}, last},
		HasMore: true,
		Total:   50,
	}}
	app := newAppWithReader(&fakeHandlerUC{}, rd, "LAWYER", "tenant-9")

	// Page 1: no cursor → sentinel, and the envelope hands back a next_cursor.
	status, body := do(t, app, http.MethodGet, "/v1/processos/cr-1/andamentos?limit=2", "", "jwt")
	if status != http.StatusOK {
		t.Fatalf("page 1 status = %d, want 200", status)
	}
	var page1 httpx.Page[AndamentoView]
	if err := json.Unmarshal([]byte(body), &page1); err != nil {
		t.Fatalf("unmarshal page 1: %v (body: %s)", err, body)
	}
	if page1.Page.NextCursor == nil {
		t.Fatal("page 1 next_cursor = nil, want a token (HasMore was true)")
	}

	// Page 2: echo the token → the handler decodes it into the last row's keyset.
	status, _ = do(t, app, http.MethodGet,
		"/v1/processos/cr-1/andamentos?limit=2&cursor="+*page1.Page.NextCursor, "", "jwt")
	if status != http.StatusOK {
		t.Fatalf("page 2 status = %d, want 200", status)
	}
	if rd.gotAndQuery.LastOccurred != last.OccurredAt.Format(time.RFC3339Nano) {
		t.Errorf("page 2 LastOccurred = %q, want %q",
			rd.gotAndQuery.LastOccurred, last.OccurredAt.Format(time.RFC3339Nano))
	}
	if rd.gotAndQuery.LastID != last.ID {
		t.Errorf("page 2 LastID = %q, want %q", rd.gotAndQuery.LastID, last.ID)
	}
}

// --- read route: /v1/processos/:id/activity ---------------------------------

// GET /v1/processos/:id/activity forwards the path :id (the court_record id) and the
// decoded ?cursor to the read port, clamps ?limit, and takes the tenant from the
// principal (never the query) — mirrors TestHandler_ListAndamentos_ForwardsProcessoAndCursor.
func TestHandler_ListActivity_ForwardsProcessoAndCursor(t *testing.T) {
	t.Parallel()

	rd := &recordingReader{}
	app := newAppWithReader(&fakeHandlerUC{}, rd, "LAWYER", "tenant-9")

	cursor := httpx.EncodeCursor(httpx.Cursor{
		LastSortValue: "2024-03-01T12:30:00Z",
		LastID:        "018f0000-0000-7000-8000-000000000abc",
	})
	status, _ := do(t, app, http.MethodGet,
		"/v1/processos/cr-77/activity?limit=25&cursor="+cursor, "", "jwt")

	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if rd.gotActQuery.CourtRecordID != "cr-77" {
		t.Errorf("CourtRecordID = %q, want cr-77 (from path)", rd.gotActQuery.CourtRecordID)
	}
	if rd.gotActQuery.TenantID != "tenant-9" {
		t.Errorf("TenantID = %q, want tenant-9 (from principal)", rd.gotActQuery.TenantID)
	}
	if rd.gotActQuery.LastOccurred != "2024-03-01T12:30:00Z" {
		t.Errorf("LastOccurred = %q, want the decoded cursor sort value", rd.gotActQuery.LastOccurred)
	}
	if rd.gotActQuery.LastID != "018f0000-0000-7000-8000-000000000abc" {
		t.Errorf("LastID = %q, want the decoded cursor id", rd.gotActQuery.LastID)
	}
	if rd.gotActQuery.Limit != 25 {
		t.Errorf("Limit = %d, want 25", rd.gotActQuery.Limit)
	}
}

// The first page passes the max sentinel cursor (no ?cursor), and ?limit defaults to
// DefaultLimit when absent.
func TestHandler_ListActivity_FirstPageSentinelAndDefaultLimit(t *testing.T) {
	t.Parallel()

	rd := &recordingReader{}
	app := newAppWithReader(&fakeHandlerUC{}, rd, "LAWYER", "tenant-9")

	status, _ := do(t, app, http.MethodGet, "/v1/processos/cr-1/activity", "", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if rd.gotActQuery.Limit != httpx.DefaultLimit {
		t.Errorf("Limit = %d, want %d (default)", rd.gotActQuery.Limit, httpx.DefaultLimit)
	}
	if rd.gotActQuery.LastOccurred != maxTimestamp || rd.gotActQuery.LastID != maxUUID {
		t.Errorf("first-page sentinel = (%q, %q), want (%q, %q)",
			rd.gotActQuery.LastOccurred, rd.gotActQuery.LastID, maxTimestamp, maxUUID)
	}
}

// A process with no activity serializes as an empty data array (never null) with the
// zero totals — 200, not 404.
func TestHandler_ListActivity_Empty(t *testing.T) {
	t.Parallel()

	rd := &recordingReader{actRes: ActivityLogResult{}}
	app := newAppWithReader(&fakeHandlerUC{}, rd, "LAWYER", "tenant-9")

	status, body := do(t, app, http.MethodGet, "/v1/processos/cr-empty/activity", "", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	for _, want := range []string{`"data":[]`, `"next_cursor":null`, `"total":0`} {
		if !strings.Contains(body, want) {
			t.Errorf("envelope missing %s\ngot: %s", want, body)
		}
	}
}

// Rows carry the pre-rendered PT-BR text — the FE never needs to map event_type itself.
func TestHandler_ListActivity_RendersText(t *testing.T) {
	t.Parallel()

	rd := &recordingReader{actRes: ActivityLogResult{
		Items: []ActivityLogView{
			{ID: "a1", EventType: ActivityEventDraftGenerated, Text: "Peça gerada", OccurredAt: time.Now()},
		},
	}}
	app := newAppWithReader(&fakeHandlerUC{}, rd, "LAWYER", "tenant-9")

	status, body := do(t, app, http.MethodGet, "/v1/processos/cr-1/activity", "", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	for _, want := range []string{`"event_type":"DRAFT_GENERATED"`, `"text":"Peça gerada"`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %s\ngot: %s", want, body)
		}
	}
}

// A malformed ?cursor is a client error → 400, not a 500.
func TestHandler_ListActivity_BadCursor_400(t *testing.T) {
	t.Parallel()

	app := newAppWithReader(&fakeHandlerUC{}, &recordingReader{}, "LAWYER", "tenant-9")
	status, body := do(t, app, http.MethodGet, "/v1/processos/cr-1/activity?cursor=not-a-cursor", "", "jwt")
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", status, body)
	}
}

// --- read route: /v1/processos/:id/intimacoes -------------------------------

// GET /v1/processos/:id/intimacoes forwards the path :id (the court_record id) and the
// decoded ?cursor to the read port, clamps ?limit, and takes the tenant from the
// principal (never the query/body).
func TestHandler_ListIntimacoesByProcesso_ForwardsProcessoAndCursor(t *testing.T) {
	t.Parallel()

	rd := &recordingReader{}
	app := newAppWithReader(&fakeHandlerUC{}, rd, "LAWYER", "tenant-9")

	cursor := httpx.EncodeCursor(httpx.Cursor{
		LastSortValue: "2024-03-01",
		LastID:        "018f0000-0000-7000-8000-000000000abc",
	})
	status, _ := do(t, app, http.MethodGet,
		"/v1/processos/cr-77/intimacoes?limit=25&cursor="+cursor, "", "jwt")

	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if rd.gotIntiQuery.CourtRecordID != "cr-77" {
		t.Errorf("CourtRecordID = %q, want cr-77 (from path)", rd.gotIntiQuery.CourtRecordID)
	}
	if rd.gotIntiQuery.TenantID != "tenant-9" {
		t.Errorf("TenantID = %q, want tenant-9 (from principal)", rd.gotIntiQuery.TenantID)
	}
	if rd.gotIntiQuery.LastMadeAvailable != "2024-03-01" {
		t.Errorf("LastMadeAvailable = %q, want the decoded cursor sort value", rd.gotIntiQuery.LastMadeAvailable)
	}
	if rd.gotIntiQuery.LastID != "018f0000-0000-7000-8000-000000000abc" {
		t.Errorf("LastID = %q, want the decoded cursor id", rd.gotIntiQuery.LastID)
	}
	if rd.gotIntiQuery.Limit != 25 {
		t.Errorf("Limit = %d, want 25", rd.gotIntiQuery.Limit)
	}
}

// The first page passes the max sentinel cursor (no ?cursor), and ?limit defaults to
// DefaultLimit when absent.
func TestHandler_ListIntimacoesByProcesso_FirstPageSentinelAndDefaultLimit(t *testing.T) {
	t.Parallel()

	rd := &recordingReader{}
	app := newAppWithReader(&fakeHandlerUC{}, rd, "LAWYER", "tenant-9")

	status, _ := do(t, app, http.MethodGet, "/v1/processos/cr-1/intimacoes", "", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if rd.gotIntiQuery.Limit != httpx.DefaultLimit {
		t.Errorf("Limit = %d, want %d (default)", rd.gotIntiQuery.Limit, httpx.DefaultLimit)
	}
	if rd.gotIntiQuery.LastMadeAvailable != maxDate || rd.gotIntiQuery.LastID != maxUUID {
		t.Errorf("first-page sentinel = (%q, %q), want (%q, %q)",
			rd.gotIntiQuery.LastMadeAvailable, rd.gotIntiQuery.LastID, maxDate, maxUUID)
	}
}

// ?limit above the ceiling is clamped to MaxLimit (100).
func TestHandler_ListIntimacoesByProcesso_ClampsLimit(t *testing.T) {
	t.Parallel()

	rd := &recordingReader{}
	app := newAppWithReader(&fakeHandlerUC{}, rd, "LAWYER", "tenant-9")

	status, _ := do(t, app, http.MethodGet, "/v1/processos/cr-1/intimacoes?limit=500", "", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if rd.gotIntiQuery.Limit != httpx.MaxLimit {
		t.Errorf("Limit = %d, want %d (clamped)", rd.gotIntiQuery.Limit, httpx.MaxLimit)
	}
}

// A process with no intimations serializes as an empty data array (never null) with the
// zero totals — 200, not 404.
func TestHandler_ListIntimacoesByProcesso_Empty(t *testing.T) {
	t.Parallel()

	rd := &recordingReader{intiRes: IntimacoesByProcessoResult{}}
	app := newAppWithReader(&fakeHandlerUC{}, rd, "LAWYER", "tenant-9")

	status, body := do(t, app, http.MethodGet, "/v1/processos/cr-empty/intimacoes", "", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	for _, want := range []string{`"data":[]`, `"next_cursor":null`, `"total":0`} {
		if !strings.Contains(body, want) {
			t.Errorf("envelope missing %s\ngot: %s", want, body)
		}
	}
}

// The response envelope carries the "X de Y" total from the read result (no search on
// this tab, so total_count and total coincide).
func TestHandler_ListIntimacoesByProcesso_EnvelopeHasTotal(t *testing.T) {
	t.Parallel()

	rd := &recordingReader{intiRes: IntimacoesByProcessoResult{
		Items: []IntimacaoView{{ID: "a", CNJNumber: "0001"}},
		Total: 12,
	}}
	app := newAppWithReader(&fakeHandlerUC{}, rd, "LAWYER", "tenant-9")

	status, body := do(t, app, http.MethodGet, "/v1/processos/cr-1/intimacoes", "", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	for _, want := range []string{`"total_count":12`, `"total":12`} {
		if !strings.Contains(body, want) {
			t.Errorf("envelope missing %s\ngot: %s", want, body)
		}
	}
}

// A malformed ?cursor is a client error → 400, not a 500.
func TestHandler_ListIntimacoesByProcesso_BadCursor_400(t *testing.T) {
	t.Parallel()

	app := newAppWithReader(&fakeHandlerUC{}, &recordingReader{}, "LAWYER", "tenant-9")
	status, body := do(t, app, http.MethodGet, "/v1/processos/cr-1/intimacoes?cursor=not-a-cursor", "", "jwt")
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", status, body)
	}
}

// Cursor round-trip across two pages: when the read reports a further page, the envelope
// carries a next_cursor keyed off the last row's (made_available_at, id) descending
// keyset; echoing it back resumes the scan exactly there.
func TestHandler_ListIntimacoesByProcesso_CursorRoundTrip(t *testing.T) {
	t.Parallel()

	last := IntimacaoView{
		ID:              "018f0000-0000-7000-8000-0000000000ff",
		MadeAvailableAt: time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC),
	}
	rd := &recordingReader{intiRes: IntimacoesByProcessoResult{
		Items:   []IntimacaoView{{ID: "first"}, last},
		HasMore: true,
		Total:   50,
	}}
	app := newAppWithReader(&fakeHandlerUC{}, rd, "LAWYER", "tenant-9")

	// Page 1: no cursor → sentinel, and the envelope hands back a next_cursor.
	status, body := do(t, app, http.MethodGet, "/v1/processos/cr-1/intimacoes?limit=2", "", "jwt")
	if status != http.StatusOK {
		t.Fatalf("page 1 status = %d, want 200", status)
	}
	var page1 httpx.Page[IntimacaoView]
	if err := json.Unmarshal([]byte(body), &page1); err != nil {
		t.Fatalf("unmarshal page 1: %v (body: %s)", err, body)
	}
	if page1.Page.NextCursor == nil {
		t.Fatal("page 1 next_cursor = nil, want a token (HasMore was true)")
	}

	// Page 2: echo the token → the handler decodes it into the last row's keyset.
	status, _ = do(t, app, http.MethodGet,
		"/v1/processos/cr-1/intimacoes?limit=2&cursor="+*page1.Page.NextCursor, "", "jwt")
	if status != http.StatusOK {
		t.Fatalf("page 2 status = %d, want 200", status)
	}
	if rd.gotIntiQuery.LastMadeAvailable != last.MadeAvailableAt.Format(time.DateOnly) {
		t.Errorf("page 2 LastMadeAvailable = %q, want %q",
			rd.gotIntiQuery.LastMadeAvailable, last.MadeAvailableAt.Format(time.DateOnly))
	}
	if rd.gotIntiQuery.LastID != last.ID {
		t.Errorf("page 2 LastID = %q, want %q", rd.gotIntiQuery.LastID, last.ID)
	}
}

// --- read route: GET /v1/intimacoes/:id (deep-link to one intimation) --------

// GET /v1/intimacoes/:id forwards the path :id and the principal's tenant (never the
// path/query) to the read port and returns the IntimacaoDetailView as the whole payload —
// 200, no list envelope. The detail carries the FULL content, judging_body and the
// triagem user_status alongside the list fields.
func TestHandler_GetIntimacao_OK(t *testing.T) {
	t.Parallel()

	rd := &recordingReader{intiOneRes: IntimacaoDetailView{
		IntimacaoView: IntimacaoView{
			ID:         "018f0000-0000-7000-8000-000000000abc",
			CNJNumber:  "0004567-11.2023.8.26.0001",
			Court:      "TJSP",
			Degree:     "G1",
			Status:     IntimationStatusActive,
			UserStatus: IntimationUserStatusPending,
			Source:     "DJEN",
		},
		Content:     "teor completo da intimação, sem truncar",
		JudgingBody: "2ª Vara Cível",
		Recipients:  json.RawMessage(`[{"name":"Fulano","matched":true}]`),
	}}
	app := newAppWithReader(&fakeHandlerUC{}, rd, "LAWYER", "tenant-9")

	status, body := do(t, app, http.MethodGet,
		"/v1/intimacoes/018f0000-0000-7000-8000-000000000abc", "", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", status, body)
	}
	if rd.gotIntiOneTID != "tenant-9" {
		t.Errorf("tenant forwarded = %q, want tenant-9 (from principal)", rd.gotIntiOneTID)
	}
	if rd.gotIntiOneID != "018f0000-0000-7000-8000-000000000abc" {
		t.Errorf("id forwarded = %q, want the path :id", rd.gotIntiOneID)
	}
	// The detail carries the list fields AND the deep-link extras (full content,
	// judging_body, recipients, user_status).
	for _, want := range []string{
		`"cnj_number":"0004567-11.2023.8.26.0001"`,
		`"user_status":"PENDING"`,
		`"content":"teor completo da intimação, sem truncar"`,
		`"judging_body":"2ª Vara Cível"`,
		`"recipients":[{"name":"Fulano","matched":true}]`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %s\ngot: %s", want, body)
		}
	}
	// The single view is returned bare, not wrapped in the list {data:[...]} envelope.
	if strings.Contains(body, `"data"`) {
		t.Errorf("GET /:id must not use the list envelope\ngot: %s", body)
	}
}

// A miss — or a foreign tenant's id — is the read model's typed ErrIntimationNotFound
// → 404 with the {kind,...} envelope, never a 500 or an empty 200.
func TestHandler_GetIntimacao_NotFound_404(t *testing.T) {
	t.Parallel()

	rd := &recordingReader{intiOneErr: ErrIntimationNotFound}
	app := newAppWithReader(&fakeHandlerUC{}, rd, "LAWYER", "tenant-9")

	status, body := do(t, app, http.MethodGet,
		"/v1/intimacoes/018f0000-0000-7000-8000-000000000abc", "", "jwt")
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body: %s)", status, body)
	}
	if !strings.Contains(body, string(apperr.KindNotFound)) {
		t.Errorf("body missing kind %q\ngot: %s", apperr.KindNotFound, body)
	}
}

// A non-uuid :id is client input → the read model's typed KindInvalid → 400, not a 500.
func TestHandler_GetIntimacao_InvalidID_400(t *testing.T) {
	t.Parallel()

	rd := &recordingReader{intiOneErr: apperr.NewInvalid("id de intimação inválido")}
	app := newAppWithReader(&fakeHandlerUC{}, rd, "LAWYER", "tenant-9")

	status, body := do(t, app, http.MethodGet, "/v1/intimacoes/not-a-uuid", "", "jwt")
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", status, body)
	}
	if !strings.Contains(body, string(apperr.KindInvalid)) {
		t.Errorf("body missing kind %q\ngot: %s", apperr.KindInvalid, body)
	}
}

// --- read route: GET /v1/processos/:id (deep-link to one process) ------------

// GET /v1/processos/:id forwards the path :id and the principal's tenant (never the
// path/query) to the read port and returns the ProcessoView as the whole payload —
// 200, no list envelope.
func TestHandler_GetProcesso_OK(t *testing.T) {
	t.Parallel()

	rd := &recordingReader{procOneRes: ProcessoView{
		ID:        "018f0000-0000-7000-8000-000000000abc",
		CNJNumber: "0004567-11.2023.8.26.0001",
		Court:     "TJSP",
		Degree:    "G1",
		Lifecycle: "ACTIVE",
	}}
	app := newAppWithReader(&fakeHandlerUC{}, rd, "LAWYER", "tenant-9")

	status, body := do(t, app, http.MethodGet,
		"/v1/processos/018f0000-0000-7000-8000-000000000abc", "", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", status, body)
	}
	if rd.gotProcOneTID != "tenant-9" {
		t.Errorf("tenant forwarded = %q, want tenant-9 (from principal)", rd.gotProcOneTID)
	}
	if rd.gotProcOneID != "018f0000-0000-7000-8000-000000000abc" {
		t.Errorf("id forwarded = %q, want the path :id", rd.gotProcOneID)
	}
	for _, want := range []string{`"cnj_number":"0004567-11.2023.8.26.0001"`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %s\ngot: %s", want, body)
		}
	}
	// The single view is returned bare, not wrapped in the list {data:[...]} envelope.
	if strings.Contains(body, `"data"`) {
		t.Errorf("GET /:id must not use the list envelope\ngot: %s", body)
	}
}

// A miss — or a foreign tenant's id — is the read model's typed ErrProcessoNotFound
// → 404 with the {kind,...} envelope, never a 500 or an empty 200.
func TestHandler_GetProcesso_NotFound_404(t *testing.T) {
	t.Parallel()

	rd := &recordingReader{procOneErr: ErrProcessoNotFound}
	app := newAppWithReader(&fakeHandlerUC{}, rd, "LAWYER", "tenant-9")

	status, body := do(t, app, http.MethodGet,
		"/v1/processos/018f0000-0000-7000-8000-000000000abc", "", "jwt")
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body: %s)", status, body)
	}
	if !strings.Contains(body, string(apperr.KindNotFound)) {
		t.Errorf("body missing kind %q\ngot: %s", apperr.KindNotFound, body)
	}
}

// A non-uuid :id is client input → the read model's typed KindInvalid → 400, not a 500.
func TestHandler_GetProcesso_InvalidID_400(t *testing.T) {
	t.Parallel()

	rd := &recordingReader{procOneErr: apperr.NewInvalid("id de processo inválido")}
	app := newAppWithReader(&fakeHandlerUC{}, rd, "LAWYER", "tenant-9")

	status, body := do(t, app, http.MethodGet, "/v1/processos/not-a-uuid", "", "jwt")
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", status, body)
	}
	if !strings.Contains(body, string(apperr.KindInvalid)) {
		t.Errorf("body missing kind %q\ngot: %s", apperr.KindInvalid, body)
	}
}

// --- PUT /v1/processos/:id/responsavel ---------------------------------------

// A valid assign body → 200 with the re-read ProcessoView (the FE reidrates the header).
// The handler forwards the principal's tenant and the path :id to the write use case,
// then reads the fresh view through the read port.
func TestHandler_AssignResponsible_OK(t *testing.T) {
	t.Parallel()

	userID := "018f0000-0000-7000-8000-0000000000aa"
	userName := "Dra. Ana"
	rd := &recordingReader{procOneRes: ProcessoView{
		ID:               "018f0000-0000-7000-8000-000000000abc",
		CNJNumber:        "0004567-11.2023.8.26.0001",
		AssignedUserID:   &userID,
		AssignedUserName: &userName,
	}}
	uc := &fakeHandlerUC{}
	app := newAppWithReader(uc, rd, "LAWYER", "tenant-9")

	body := `{"user_id":"` + userID + `"}`
	status, resp := do(t, app, http.MethodPut,
		"/v1/processos/018f0000-0000-7000-8000-000000000abc/responsavel", body, "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", status, resp)
	}
	if uc.gotAssignTenant != "tenant-9" {
		t.Errorf("tenant forwarded = %q, want tenant-9 (from principal)", uc.gotAssignTenant)
	}
	if uc.gotAssignRecord != "018f0000-0000-7000-8000-000000000abc" {
		t.Errorf("record forwarded = %q, want the path :id", uc.gotAssignRecord)
	}
	if uc.gotAssignUser == nil || *uc.gotAssignUser != userID {
		t.Errorf("user forwarded = %v, want %q", uc.gotAssignUser, userID)
	}
	// The re-read view is the whole payload (with the fresh responsável), no list envelope.
	for _, want := range []string{`"assigned_user_id":"` + userID + `"`, `"assigned_user_name":"Dra. Ana"`} {
		if !strings.Contains(resp, want) {
			t.Errorf("body missing %s\ngot: %s", want, resp)
		}
	}
}

// A null user_id (desatribuir) is valid → 200, and the handler forwards a nil user.
func TestHandler_AssignResponsible_Unassign_OK(t *testing.T) {
	t.Parallel()

	rd := &recordingReader{procOneRes: ProcessoView{ID: "018f0000-0000-7000-8000-000000000abc"}}
	uc := &fakeHandlerUC{}
	app := newAppWithReader(uc, rd, "LAWYER", "tenant-9")

	status, resp := do(t, app, http.MethodPut,
		"/v1/processos/018f0000-0000-7000-8000-000000000abc/responsavel", `{"user_id":null}`, "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", status, resp)
	}
	if uc.gotAssignUser != nil {
		t.Errorf("user forwarded = %v, want nil (desatribuir)", uc.gotAssignUser)
	}
}

// A malformed user_id (not a uuid) is rejected at the edge by Validate → 400, before the
// use case is ever called.
func TestHandler_AssignResponsible_InvalidBody_400(t *testing.T) {
	t.Parallel()

	uc := &fakeHandlerUC{}
	app := newAppWithReader(uc, &recordingReader{}, "LAWYER", "tenant-9")

	status, body := do(t, app, http.MethodPut,
		"/v1/processos/018f0000-0000-7000-8000-000000000abc/responsavel", `{"user_id":"not-a-uuid"}`, "jwt")
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", status, body)
	}
	if uc.gotAssignRecord != "" {
		t.Errorf("use case was called on an invalid body (record=%q)", uc.gotAssignRecord)
	}
}

// --- bulk responsável route: POST /v1/processos/bulk/responsavel -------------

// The "por ids" mode forwards All=false, the ids list and the assignee straight to
// the use case, and answers 200 with the affected count the use case returned.
func TestHandler_BulkAssignResponsible_ByIDs_OK(t *testing.T) {
	t.Parallel()

	userID := "018f0000-0000-7000-8000-0000000000aa"
	recordID1 := "018f0000-0000-7000-8000-000000000cc1"
	recordID2 := "018f0000-0000-7000-8000-000000000cc2"
	uc := &fakeHandlerUC{bulkResp: 2}
	app := newApp(uc, "LAWYER", "tenant-9")

	body := `{"user_id":"` + userID + `","ids":["` + recordID1 + `","` + recordID2 + `"]}`
	status, resp := do(t, app, http.MethodPost, "/v1/processos/bulk/responsavel", body, "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", status, resp)
	}
	if uc.gotBulkAll {
		t.Errorf("all forwarded = true, want false (por-ids mode)")
	}
	if len(uc.gotBulkIDs) != 2 || uc.gotBulkIDs[0] != recordID1 || uc.gotBulkIDs[1] != recordID2 {
		t.Errorf("ids forwarded = %v, want [%s %s]", uc.gotBulkIDs, recordID1, recordID2)
	}
	if uc.gotBulkUser == nil || *uc.gotBulkUser != userID {
		t.Errorf("user forwarded = %v, want %q", uc.gotBulkUser, userID)
	}
	if !strings.Contains(resp, `"affected":2`) {
		t.Errorf("body = %s, want affected:2", resp)
	}
}

// All=true forwards the ProcessosQuery filters (search/court/lifecycle/degree/assignee)
// mirroring GET /processos, and ids stays empty — the "all" mode ignores it.
func TestHandler_BulkAssignResponsible_All_ForwardsFilters(t *testing.T) {
	t.Parallel()

	assigneeID := "018f0000-0000-7000-8000-0000000000bb"
	uc := &fakeHandlerUC{bulkResp: 7}
	app := newApp(uc, "LAWYER", "tenant-9")

	body := `{"all":true,"search":"0004567","court":"TJSP","lifecycle":"ACTIVE","degree":"1","assignee":"` + assigneeID + `"}`
	status, resp := do(t, app, http.MethodPost, "/v1/processos/bulk/responsavel", body, "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", status, resp)
	}
	if !uc.gotBulkAll {
		t.Fatalf("all forwarded = false, want true")
	}
	want := ProcessosQuery{
		TenantID: "tenant-9", Search: "0004567", Court: "TJSP",
		Lifecycle: "ACTIVE", Degree: "1", Assignee: assigneeID,
	}
	if uc.gotBulkQuery != want {
		t.Errorf("query forwarded = %+v, want %+v", uc.gotBulkQuery, want)
	}
	if !strings.Contains(resp, `"affected":7`) {
		t.Errorf("body = %s, want affected:7", resp)
	}
}

// A null user_id (desatribuir em massa) is valid → 200, and the handler forwards nil.
func TestHandler_BulkAssignResponsible_Unassign_OK(t *testing.T) {
	t.Parallel()

	uc := &fakeHandlerUC{bulkResp: 1}
	app := newApp(uc, "LAWYER", "tenant-9")

	status, resp := do(t, app, http.MethodPost, "/v1/processos/bulk/responsavel",
		`{"user_id":null,"ids":["018f0000-0000-7000-8000-000000000cc1"]}`, "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", status, resp)
	}
	if uc.gotBulkUser != nil {
		t.Errorf("user forwarded = %v, want nil (desatribuir)", uc.gotBulkUser)
	}
}

// The por-ids mode requires at least one id — an empty list is a 400 at the edge,
// before the use case is ever called.
func TestHandler_BulkAssignResponsible_EmptyIDs_400(t *testing.T) {
	t.Parallel()

	uc := &fakeHandlerUC{}
	app := newApp(uc, "LAWYER", "tenant-9")

	status, body := do(t, app, http.MethodPost, "/v1/processos/bulk/responsavel",
		`{"ids":[]}`, "jwt")
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", status, body)
	}
	if uc.gotBulkIDs != nil {
		t.Errorf("use case was called on an invalid body (ids=%v)", uc.gotBulkIDs)
	}
}

// A malformed user_id (not a uuid) is rejected at the edge by Validate → 400, before
// the use case is ever called.
func TestHandler_BulkAssignResponsible_InvalidUserID_400(t *testing.T) {
	t.Parallel()

	uc := &fakeHandlerUC{}
	app := newApp(uc, "LAWYER", "tenant-9")

	status, body := do(t, app, http.MethodPost, "/v1/processos/bulk/responsavel",
		`{"user_id":"not-a-uuid","ids":["cr-1"]}`, "jwt")
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", status, body)
	}
	if uc.gotBulkTenant != "" {
		t.Errorf("use case was called on an invalid body (tenant=%q)", uc.gotBulkTenant)
	}
}

// tenant_id sempre vem do principal, nunca do body: the request shape has no
// tenant field at all, so even a malicious "tenant_id" key in the raw JSON is a
// no-op — the handler forwards httpx.TenantFromCtx(c), not anything client-supplied.
func TestHandler_BulkAssignResponsible_TenantFromPrincipal_NeverBody(t *testing.T) {
	t.Parallel()

	uc := &fakeHandlerUC{bulkResp: 1}
	app := newApp(uc, "LAWYER", "tenant-9")

	body := `{"tenant_id":"attacker-tenant","ids":["018f0000-0000-7000-8000-000000000cc1"]}`
	status, resp := do(t, app, http.MethodPost, "/v1/processos/bulk/responsavel", body, "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", status, resp)
	}
	if uc.gotBulkTenant != "tenant-9" {
		t.Fatalf("tenant forwarded = %q, want tenant-9 (from principal, never the body)", uc.gotBulkTenant)
	}
	if uc.gotBulkQuery.TenantID != "tenant-9" {
		t.Fatalf("query tenant = %q, want tenant-9", uc.gotBulkQuery.TenantID)
	}
}

// --- triagem routes: POST /v1/intimacoes/:id/{resolve,ignore,reopen} ---------

// The three triagem verbs forward the path :id and the principal's tenant to the write use
// case (the right verb per route), then return the re-read detail view — 200, no envelope.
func TestHandler_TriageIntimacao_Verbs_OK(t *testing.T) {
	t.Parallel()

	const id = "018f0000-0000-7000-8000-000000000abc"
	tests := []struct {
		route    string
		wantVerb string
	}{
		{"resolve", "resolve"},
		{"ignore", "ignore"},
		{"reopen", "reopen"},
	}

	for _, tt := range tests {
		t.Run(tt.route, func(t *testing.T) {
			t.Parallel()

			rd := &recordingReader{intiOneRes: IntimacaoDetailView{
				IntimacaoView: IntimacaoView{ID: id, UserStatus: IntimationUserStatusResolved},
			}}
			uc := &fakeHandlerUC{}
			app := newAppWithReader(uc, rd, "LAWYER", "tenant-9")

			status, body := do(t, app, http.MethodPost, "/v1/intimacoes/"+id+"/"+tt.route, "", "jwt")
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body: %s)", status, body)
			}
			if uc.gotTriageVerb != tt.wantVerb {
				t.Errorf("verb called = %q, want %q", uc.gotTriageVerb, tt.wantVerb)
			}
			if uc.gotTriageTenant != "tenant-9" || uc.gotTriageID != id {
				t.Errorf("forwarded (tenant, id) = (%q, %q), want (tenant-9, %q)", uc.gotTriageTenant, uc.gotTriageID, id)
			}
			// The response is the fresh detail view (re-read), not a list envelope.
			if strings.Contains(body, `"data"`) {
				t.Errorf("triagem response must not use the list envelope\ngot: %s", body)
			}
		})
	}
}

// A miss/foreign id from the write use case is the typed ErrIntimationNotFound → 404, and
// the read (re-read) is never reached.
func TestHandler_TriageIntimacao_NotFound_404(t *testing.T) {
	t.Parallel()

	const id = "018f0000-0000-7000-8000-000000000abc"
	uc := &fakeHandlerUC{triageErr: ErrIntimationNotFound}
	app := newAppWithReader(uc, &recordingReader{}, "LAWYER", "tenant-9")

	status, body := do(t, app, http.MethodPost, "/v1/intimacoes/"+id+"/resolve", "", "jwt")
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body: %s)", status, body)
	}
}

// --- summary routes: GET /v1/{processos,intimacoes}/summary ------------------

// GET /v1/processos/summary returns the bucketed lifecycle counts as a bare read model
// (no list envelope), tenant-scoped.
func TestHandler_ProcessosSummary_OK(t *testing.T) {
	t.Parallel()

	rd := &recordingReader{procSummary: ProcessosSummaryView{
		Total: 42, EmAndamento: 30, Suspensos: 5, Arquivados: 7, Baixados: 0,
	}}
	app := newAppWithReader(&fakeHandlerUC{}, rd, "LAWYER", "tenant-9")

	status, body := do(t, app, http.MethodGet, "/v1/processos/summary", "", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", status, body)
	}
	for _, want := range []string{`"total":42`, `"em_andamento":30`, `"suspensos":5`, `"arquivados":7`, `"baixados":0`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %s\ngot: %s", want, body)
		}
	}
	if strings.Contains(body, `"data"`) {
		t.Errorf("summary must not use the list envelope\ngot: %s", body)
	}
}

// GET /v1/intimacoes/summary returns the triagem-bucketed counts (em_analise/criticas are
// 0 for now) as a bare read model. It must NOT be shadowed by the /:id param route.
func TestHandler_IntimacoesSummary_OK(t *testing.T) {
	t.Parallel()

	rd := &recordingReader{intiSummary: IntimacoesSummaryView{
		Total: 20, Pendentes: 12, EmAnalise: 0, Resolvidas: 6, Ignoradas: 2, Criticas: 0,
	}}
	app := newAppWithReader(&fakeHandlerUC{}, rd, "LAWYER", "tenant-9")

	status, body := do(t, app, http.MethodGet, "/v1/intimacoes/summary", "", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", status, body)
	}
	// If the :id route had shadowed /summary, "summary" would reach GetIntimacao as an id
	// and yield a 400 (non-uuid) — so a 200 with these buckets proves the route order.
	for _, want := range []string{`"total":20`, `"pendentes":12`, `"resolvidas":6`, `"ignoradas":2`, `"em_analise":0`, `"criticas":0`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %s\ngot: %s", want, body)
		}
	}
}

// fakeResumer is the resumer port fake for the /resume route.
type fakeResumer struct {
	view ProcessResumoView
	err  error
}

func (f fakeResumer) Resume(_ context.Context, _, _ string) (ProcessResumoView, error) {
	return f.view, f.err
}

// GET /v1/processos/:id/resume returns the AI summary view when the resumer port is
// wired. tenant comes from the principal; the resumer is invoked with it.
func TestHandler_Resume_OK(t *testing.T) {
	t.Parallel()

	rs := fakeResumer{view: ProcessResumoView{
		Summary:              "Processo em fase de contestação",
		CurrentStatus:        "Aguardando manifestação",
		KeyDatesAndDeadlines: []KeyDate{{Kind: "MANIFESTACAO", EndDate: "2026-08-04", DaysRemaining: 3, Urgency: "DUE_SOON", Source: "deadline"}},
		RecentMovements:      []RecentMovement{},
		Risks:                []Risk{},
		RecommendedActions:   []RecommendedAction{},
		GeneratedAt:          time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC),
	}}

	app := fiber.New(fiber.Config{
		ErrorHandler: func(c *fiber.Ctx, err error) error { return httpx.WriteError(c, err) },
	})
	v1 := app.Group("/v1", middleware.Auth(stubVerifier{}, stubResolver{role: "LAWYER", tenant: "tenant-9"}))
	NewHandler(&fakeHandlerUC{}, &recordingReader{}, rs, nil, nil, nil).RegisterV1(v1)

	status, body := do(t, app, http.MethodGet, "/v1/processos/rec-1/resume", "", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", status, body)
	}
	for _, want := range []string{`"summary":"Processo em fase de contestação"`, `"current_status":"Aguardando manifestação"`, `"days_remaining":3`, `"key_dates_and_deadlines":`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %s\ngot: %s", want, body)
		}
	}
	// Slices must serialize as [] (never null) for a stable FE shape.
	if strings.Contains(body, `"recent_movements":null`) || strings.Contains(body, `"risks":null`) {
		t.Errorf("slices serialized as null\ngot: %s", body)
	}
}

// GET /v1/processos/:id/resume with no resumer wired is a typed 501 — the route
// exists but no provider is configured.
func TestHandler_Resume_NoResumer_501(t *testing.T) {
	t.Parallel()

	app := fiber.New(fiber.Config{
		ErrorHandler: func(c *fiber.Ctx, err error) error { return httpx.WriteError(c, err) },
	})
	v1 := app.Group("/v1", middleware.Auth(stubVerifier{}, stubResolver{role: "LAWYER", tenant: "tenant-9"}))
	NewHandler(&fakeHandlerUC{}, &recordingReader{}, nil, nil, nil, nil).RegisterV1(v1)

	status, body := do(t, app, http.MethodGet, "/v1/processos/rec-1/resume", "", "jwt")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body: %s)", status, body)
	}
	if !strings.Contains(body, `"kind":"SERVICE_UNAVAILABLE"`) {
		t.Errorf("body missing kind SERVICE_UNAVAILABLE\ngot: %s", body)
	}
}

// GET /v1/intimacoes: the envelope must carry a `buckets` object with the seven
// disjoint section counts (atraso, hoje, proximos_dois_dias, esta_semana, este_mes,
// mais_adiante, sem_data_definida).
func TestHandler_ListIntimacoes_EnvelopeCarriesBuckets(t *testing.T) {
	t.Parallel()

	buckets := IntimacaoBucketsView{
		Atraso:           3,
		Hoje:             1,
		ProximosDoisDias: 2,
		EstaSemana:       5,
		EsteMes:          8,
		MaisAdiante:      12,
		SemDataDefinida:  4,
	}
	rd := &recordingReader{intiListRes: IntimacoesResult{Buckets: buckets}}
	app := newAppWithReader(&fakeHandlerUC{}, rd, "LAWYER", "tenant-9")

	status, body := do(t, app, http.MethodGet, "/v1/intimacoes", "", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, body)
	}

	var env intimacoesEnvelope
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decode envelope: %v; body=%s", err, body)
	}
	if env.Buckets != buckets {
		t.Errorf("buckets = %+v, want %+v", env.Buckets, buckets)
	}
}

// ?urgencia=mais_adiante is accepted as a valid value (closed-set) and forwarded
// to the read port; the handler returns 200.
func TestHandler_ListIntimacoes_MaisAdiante_AcceptedAndForwarded(t *testing.T) {
	t.Parallel()

	rd := &recordingReader{}
	app := newAppWithReader(&fakeHandlerUC{}, rd, "LAWYER", "tenant-9")

	status, body := do(t, app, http.MethodGet, "/v1/intimacoes?urgencia=mais_adiante", "", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, body)
	}
	if rd.gotIntiListQ.Urgencia != UrgenciaMaisAdiante {
		t.Errorf("forwarded Urgencia = %q, want %q", rd.gotIntiListQ.Urgencia, UrgenciaMaisAdiante)
	}
}

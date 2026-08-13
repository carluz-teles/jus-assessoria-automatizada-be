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

func (fakeReader) Intimacao(context.Context, string, string) (IntimacaoView, error) {
	return IntimacaoView{}, nil
}

func (fakeReader) Andamentos(context.Context, AndamentosQuery) (AndamentosResult, error) {
	return AndamentosResult{}, nil
}

func (fakeReader) IntimacoesByProcesso(context.Context, IntimacoesByProcessoQuery) (IntimacoesByProcessoResult, error) {
	return IntimacoesByProcessoResult{}, nil
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
	res          ProcessosResult
	gotQuery     ProcessosQuery
	andRes       AndamentosResult
	gotAndQuery  AndamentosQuery
	intiRes      IntimacoesByProcessoResult
	gotIntiQuery IntimacoesByProcessoQuery
	// GET /v1/intimacoes/:id — capture the forwarded (tenant, id) and return a canned
	// view or a typed error (a nil intiOneErr means the view is returned).
	intiOneRes    IntimacaoView
	intiOneErr    error
	gotIntiOneTID string
	gotIntiOneID  string
}

func (r *recordingReader) Processos(_ context.Context, q ProcessosQuery) (ProcessosResult, error) {
	r.gotQuery = q
	return r.res, nil
}
func (r *recordingReader) Intimacoes(context.Context, IntimacoesQuery) (IntimacoesResult, error) {
	return IntimacoesResult{}, nil
}
func (r *recordingReader) Intimacao(_ context.Context, tenantID, id string) (IntimacaoView, error) {
	r.gotIntiOneTID, r.gotIntiOneID = tenantID, id
	return r.intiOneRes, r.intiOneErr
}
func (r *recordingReader) Andamentos(_ context.Context, q AndamentosQuery) (AndamentosResult, error) {
	r.gotAndQuery = q
	return r.andRes, nil
}
func (r *recordingReader) IntimacoesByProcesso(_ context.Context, q IntimacoesByProcessoQuery) (IntimacoesByProcessoResult, error) {
	r.gotIntiQuery = q
	return r.intiRes, nil
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
// path/query) to the read port and returns the IntimacaoView as the whole payload —
// 200, no list envelope.
func TestHandler_GetIntimacao_OK(t *testing.T) {
	t.Parallel()

	rd := &recordingReader{intiOneRes: IntimacaoView{
		ID:        "018f0000-0000-7000-8000-000000000abc",
		CNJNumber: "0004567-11.2023.8.26.0001",
		Court:     "TJSP",
		Degree:    "G1",
		Status:    "PENDING",
		Source:    "DJEN",
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
	for _, want := range []string{`"cnj_number":"0004567-11.2023.8.26.0001"`, `"status":"PENDING"`} {
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

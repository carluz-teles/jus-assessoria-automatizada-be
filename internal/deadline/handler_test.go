package deadline

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

// stubResolver returns a principal with the configured tenant, standing in for the
// identity slice's resolver.
type stubResolver struct{ tenant string }

func (r stubResolver) Resolve(context.Context, string, string) (httpx.Principal, error) {
	return httpx.Principal{UserID: "u-1", TenantID: r.tenant, Role: "LAWYER"}, nil
}

// recordingReader implements the handler's reader port, capturing the queries the
// handler forwards and returning canned results/errors.
type recordingReader struct {
	byProcRes    PrazosByProcessoResult
	gotByProcQ   PrazosByProcessoQuery
	agendaRes    PrazosResult
	gotAgendaQ   PrazosQuery
	detailView   PrazoDetailView
	detailErr    error
	gotDetailTID string
	gotDetailID  string
}

func (r *recordingReader) PrazosByProcesso(_ context.Context, q PrazosByProcessoQuery) (PrazosByProcessoResult, error) {
	r.gotByProcQ = q
	return r.byProcRes, nil
}

func (r *recordingReader) Prazos(_ context.Context, q PrazosQuery) (PrazosResult, error) {
	r.gotAgendaQ = q
	return r.agendaRes, nil
}

func (r *recordingReader) Prazo(_ context.Context, tenantID, id string) (PrazoDetailView, error) {
	r.gotDetailTID, r.gotDetailID = tenantID, id
	return r.detailView, r.detailErr
}

// recordingWriter implements the handler's writer port, capturing the confirm command the
// handler forwards and returning a canned result/error.
type recordingWriter struct {
	gotCmd ConfirmCommand
	calls  int
	res    ConfirmResult
	err    error
}

func (w *recordingWriter) Confirm(_ context.Context, cmd ConfirmCommand) (ConfirmResult, error) {
	w.calls++
	w.gotCmd = cmd
	return w.res, w.err
}

// newApp builds an app whose /v1 group mirrors production: Auth resolves a principal with
// the given tenant, then the deadline routes mount under it. It uses a throwaway writer —
// the read tests never hit the confirm route; newAppWithWriter injects a specific one.
func newApp(rd reader, tenant string) *fiber.App {
	return newAppWithWriter(rd, &recordingWriter{}, tenant)
}

// newAppWithWriter is newApp with an explicit writer, for the confirm-route tests.
func newAppWithWriter(rd reader, wr writer, tenant string) *fiber.App {
	app := fiber.New(fiber.Config{
		ErrorHandler: func(c *fiber.Ctx, err error) error { return httpx.WriteError(c, err) },
	})
	v1 := app.Group("/v1", middleware.Auth(stubVerifier{}, stubResolver{tenant: tenant}))
	NewHandler(rd, wr).RegisterV1(v1)
	return app
}

// do drives one request through app, returning status and raw body.
func do(t *testing.T, app *fiber.App, method, path, bearer string) (int, string) {
	t.Helper()

	req := httptest.NewRequest(method, path, nil)
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

// --- auth boundary -----------------------------------------------------------

// No bearer token → 401 at the auth boundary; the handler never runs.
func TestHandler_Prazos_NoToken_401(t *testing.T) {
	t.Parallel()

	app := newApp(&recordingReader{}, "tenant-1")
	status, _ := do(t, app, http.MethodGet, "/v1/prazos", "")
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
}

// --- GET /v1/processos/:id/prazos -------------------------------------------

// The handler forwards the path :id (the court_record id) and the decoded ?cursor to the
// read port, clamps ?limit, and takes the tenant from the principal (never the query).
func TestHandler_ListPrazosByProcesso_ForwardsProcessoAndCursor(t *testing.T) {
	t.Parallel()

	rd := &recordingReader{}
	app := newApp(rd, "tenant-9")

	cursor := httpx.EncodeCursor(httpx.Cursor{
		LastSortValue: "2024-03-11",
		LastID:        "018f0000-0000-7000-8000-000000000abc",
	})
	status, _ := do(t, app, http.MethodGet,
		"/v1/processos/cr-77/prazos?limit=25&cursor="+cursor, "jwt")

	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if rd.gotByProcQ.CourtRecordID != "cr-77" {
		t.Errorf("CourtRecordID = %q, want cr-77 (from path)", rd.gotByProcQ.CourtRecordID)
	}
	if rd.gotByProcQ.TenantID != "tenant-9" {
		t.Errorf("TenantID = %q, want tenant-9 (from principal)", rd.gotByProcQ.TenantID)
	}
	if rd.gotByProcQ.LastEnd != "2024-03-11" || rd.gotByProcQ.LastID != "018f0000-0000-7000-8000-000000000abc" {
		t.Errorf("keyset = (%q, %q), want the decoded cursor", rd.gotByProcQ.LastEnd, rd.gotByProcQ.LastID)
	}
	if rd.gotByProcQ.Limit != 25 {
		t.Errorf("Limit = %d, want 25", rd.gotByProcQ.Limit)
	}
}

// The first page passes the min sentinel cursor (no ?cursor) and ?limit defaults to
// DefaultLimit when absent — the handler never asks the repo for an unbounded page.
func TestHandler_ListPrazosByProcesso_FirstPageSentinelAndDefaultLimit(t *testing.T) {
	t.Parallel()

	rd := &recordingReader{}
	app := newApp(rd, "tenant-9")

	status, _ := do(t, app, http.MethodGet, "/v1/processos/cr-1/prazos", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if rd.gotByProcQ.Limit != httpx.DefaultLimit {
		t.Errorf("Limit = %d, want %d (default)", rd.gotByProcQ.Limit, httpx.DefaultLimit)
	}
	if rd.gotByProcQ.LastEnd != minDate || rd.gotByProcQ.LastID != zeroUUID {
		t.Errorf("first-page sentinel = (%q, %q), want (%q, %q)",
			rd.gotByProcQ.LastEnd, rd.gotByProcQ.LastID, minDate, zeroUUID)
	}
}

// The row carries the FE fields (days_left, confirmed) and the envelope the total; a
// process with prazos serializes them, ordered as the read model returned (end_date asc).
func TestHandler_ListPrazosByProcesso_EnvelopeAndRowFields(t *testing.T) {
	t.Parallel()

	rd := &recordingReader{byProcRes: PrazosByProcessoResult{
		Items: []PrazoView{{
			ID: "d-1", Kind: "MANIFESTACAO", EndDate: time.Date(2024, 3, 11, 0, 0, 0, 0, time.UTC),
			DaysLeft: 3, Counting: "BUSINESS", Status: "PENDING",
			HolidaysApplied: []string{"2024-03-06"}, IntimationID: "i-1", Confirmed: false,
		}},
		Total: 1,
	}}
	app := newApp(rd, "tenant-9")

	status, body := do(t, app, http.MethodGet, "/v1/processos/cr-1/prazos", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	for _, want := range []string{
		`"days_left":3`, `"confirmed":false`, `"intimation_id":"i-1"`,
		`"holidays_applied":["2024-03-06"]`, `"total":1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %s\ngot: %s", want, body)
		}
	}
}

// A malformed ?cursor is a client error → 400, not a 500.
func TestHandler_ListPrazosByProcesso_BadCursor_400(t *testing.T) {
	t.Parallel()

	app := newApp(&recordingReader{}, "tenant-9")
	status, _ := do(t, app, http.MethodGet, "/v1/processos/cr-1/prazos?cursor=not-a-cursor", "jwt")
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
}

// --- GET /v1/prazos (agenda) -------------------------------------------------

// The handler forwards ?status and the ?from/?to window (validated) to the read port,
// and the tenant comes from the principal.
func TestHandler_ListPrazos_ForwardsFilters(t *testing.T) {
	t.Parallel()

	rd := &recordingReader{}
	app := newApp(rd, "tenant-9")

	status, _ := do(t, app, http.MethodGet,
		"/v1/prazos?status=PENDING&from=2024-03-01&to=2024-03-31&limit=10", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if rd.gotAgendaQ.Status != "PENDING" {
		t.Errorf("Status = %q, want PENDING", rd.gotAgendaQ.Status)
	}
	if rd.gotAgendaQ.From != "2024-03-01" || rd.gotAgendaQ.To != "2024-03-31" {
		t.Errorf("window = (%q, %q), want (2024-03-01, 2024-03-31)", rd.gotAgendaQ.From, rd.gotAgendaQ.To)
	}
	if rd.gotAgendaQ.TenantID != "tenant-9" {
		t.Errorf("TenantID = %q, want tenant-9 (from principal)", rd.gotAgendaQ.TenantID)
	}
}

// An unknown ?status is a client error → 400 (the read port is never called).
func TestHandler_ListPrazos_BadStatus_400(t *testing.T) {
	t.Parallel()

	rd := &recordingReader{}
	app := newApp(rd, "tenant-9")

	status, body := do(t, app, http.MethodGet, "/v1/prazos?status=BOGUS", "jwt")
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", status, body)
	}
}

// A malformed ?from date is a client error → 400.
func TestHandler_ListPrazos_BadDate_400(t *testing.T) {
	t.Parallel()

	app := newApp(&recordingReader{}, "tenant-9")
	status, _ := do(t, app, http.MethodGet, "/v1/prazos?from=03-2024", "jwt")
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
}

// The envelope carries the "X de Y" totals and the agenda's process context (cnj/court).
func TestHandler_ListPrazos_EnvelopeHasTotalsAndContext(t *testing.T) {
	t.Parallel()

	rd := &recordingReader{agendaRes: PrazosResult{
		Items: []AgendaPrazoView{{
			ID: "d-1", EndDate: time.Date(2024, 3, 11, 0, 0, 0, 0, time.UTC),
			Status: "PENDING", CNJNumber: "0001", Court: "TJSP", CourtRecordID: "cr-1",
		}},
		TotalCount: 3,
		Total:      50,
	}}
	app := newApp(rd, "tenant-9")

	status, body := do(t, app, http.MethodGet, "/v1/prazos?status=PENDING", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	for _, want := range []string{`"total_count":3`, `"total":50`, `"cnj_number":"0001"`, `"court":"TJSP"`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %s\ngot: %s", want, body)
		}
	}
}

// Cursor round-trip across two pages: when the read reports a further page, the envelope
// carries a next_cursor keyed off the last row's (end_date, id); echoing it back resumes
// the keyset exactly there.
func TestHandler_ListPrazos_CursorRoundTrip(t *testing.T) {
	t.Parallel()

	last := AgendaPrazoView{
		ID:      "018f0000-0000-7000-8000-0000000000ff",
		EndDate: time.Date(2024, 3, 20, 0, 0, 0, 0, time.UTC),
	}
	rd := &recordingReader{agendaRes: PrazosResult{
		Items:   []AgendaPrazoView{{ID: "first"}, last},
		HasMore: true,
		Total:   50,
	}}
	app := newApp(rd, "tenant-9")

	status, body := do(t, app, http.MethodGet, "/v1/prazos?limit=2", "jwt")
	if status != http.StatusOK {
		t.Fatalf("page 1 status = %d, want 200", status)
	}
	var page1 httpx.Page[AgendaPrazoView]
	if err := json.Unmarshal([]byte(body), &page1); err != nil {
		t.Fatalf("unmarshal page 1: %v (body: %s)", err, body)
	}
	if page1.Page.NextCursor == nil {
		t.Fatal("page 1 next_cursor = nil, want a token (HasMore was true)")
	}

	status, _ = do(t, app, http.MethodGet, "/v1/prazos?limit=2&cursor="+*page1.Page.NextCursor, "jwt")
	if status != http.StatusOK {
		t.Fatalf("page 2 status = %d, want 200", status)
	}
	if rd.gotAgendaQ.LastEnd != "2024-03-20" || rd.gotAgendaQ.LastID != last.ID {
		t.Errorf("page 2 keyset = (%q, %q), want (2024-03-20, %q)", rd.gotAgendaQ.LastEnd, rd.gotAgendaQ.LastID, last.ID)
	}
}

// An empty agenda serializes as an empty data array (never null) with zero totals — 200.
func TestHandler_ListPrazos_Empty(t *testing.T) {
	t.Parallel()

	app := newApp(&recordingReader{}, "tenant-9")
	status, body := do(t, app, http.MethodGet, "/v1/prazos", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	for _, want := range []string{`"data":[]`, `"next_cursor":null`, `"total":0`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %s\ngot: %s", want, body)
		}
	}
}

// --- GET /v1/prazos/:id (detail) --------------------------------------------

// The detail endpoint returns the audit view (with rules_version and full holidays) and
// forwards the tenant from the principal.
func TestHandler_GetPrazo_OK(t *testing.T) {
	t.Parallel()

	rd := &recordingReader{detailView: PrazoDetailView{
		ID: "d-1", Kind: "CONTESTACAO", Status: "PENDING", RulesVersion: "v0",
		HolidaysApplied: []string{"2024-03-06"}, IntimationID: "i-1", Days: 15, DaysLeft: 4,
	}}
	app := newApp(rd, "tenant-9")

	status, body := do(t, app, http.MethodGet, "/v1/prazos/d-1", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", status, body)
	}
	if rd.gotDetailTID != "tenant-9" || rd.gotDetailID != "d-1" {
		t.Errorf("forwarded (tenant, id) = (%q, %q), want (tenant-9, d-1)", rd.gotDetailTID, rd.gotDetailID)
	}
	for _, want := range []string{`"rules_version":"v0"`, `"days":15`, `"days_left":4`, `"intimation_id":"i-1"`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %s\ngot: %s", want, body)
		}
	}
}

// A miss is the repo's typed ErrDeadlineNotFound → 404 with the {kind,...} envelope.
func TestHandler_GetPrazo_NotFound_404(t *testing.T) {
	t.Parallel()

	rd := &recordingReader{detailErr: ErrDeadlineNotFound}
	app := newApp(rd, "tenant-9")

	status, body := do(t, app, http.MethodGet, "/v1/prazos/missing", "jwt")
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body: %s)", status, body)
	}
	if !strings.Contains(body, string(apperr.KindNotFound)) {
		t.Errorf("body missing kind %q\ngot: %s", apperr.KindNotFound, body)
	}
}

// --- POST /v1/prazos/confirm -------------------------------------------------

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

// The confirm handler takes tenant_id/confirmed_by from the PRINCIPAL (never the body),
// forwards the mapped command, and returns 200 with the confirmed prazo.
func TestHandler_Confirm_ForwardsCommandFromPrincipal(t *testing.T) {
	t.Parallel()

	wr := &recordingWriter{res: ConfirmResult{Deadline: ConfirmedDeadline{ID: "d-1", Status: StatusOpen}}}
	app := newAppWithWriter(&recordingReader{}, wr, "tenant-9")

	body := `{"intimation_id":"018f0000-0000-7000-8000-000000000abc",
		"deadline":{"kind":"CONTESTACAO","days":15,"counting":"BUSINESS","doubled":false},
		"tasks":[{"title":"Contestar","due_date":"2024-02-01"}]}`
	status, resBody := doJSON(t, app, http.MethodPost, "/v1/prazos/confirm", "jwt", body)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", status, resBody)
	}
	if wr.calls != 1 {
		t.Fatalf("writer calls = %d, want 1", wr.calls)
	}
	cmd := wr.gotCmd
	if cmd.TenantID != "tenant-9" || cmd.UserID != "u-1" {
		t.Errorf("tenant/user = %q/%q, want tenant-9/u-1 (from principal)", cmd.TenantID, cmd.UserID)
	}
	if cmd.IntimationID != "018f0000-0000-7000-8000-000000000abc" || cmd.Days != 15 || cmd.Counting != CountingBusiness {
		t.Errorf("command = %+v", cmd)
	}
	if len(cmd.Tasks) != 1 || cmd.Tasks[0].Title != "Contestar" || cmd.Tasks[0].DueDate == nil {
		t.Errorf("tasks = %+v, want one dated 'Contestar'", cmd.Tasks)
	}
	if !strings.Contains(resBody, `"id":"d-1"`) || !strings.Contains(resBody, `"status":"OPEN"`) {
		t.Errorf("response missing confirmed deadline\ngot: %s", resBody)
	}
}

// A malformed body (bad counting, non-positive days, empty task title, bad uuid) is a 400
// with the {kind,...} envelope, and the use case is never called.
func TestHandler_Confirm_Validation_400(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
	}{
		{"bad counting", `{"intimation_id":"018f0000-0000-7000-8000-000000000abc","deadline":{"days":15,"counting":"WEEKLY"},"tasks":[]}`},
		{"days zero", `{"intimation_id":"018f0000-0000-7000-8000-000000000abc","deadline":{"days":0,"counting":"BUSINESS"},"tasks":[]}`},
		{"empty task title", `{"intimation_id":"018f0000-0000-7000-8000-000000000abc","deadline":{"days":5,"counting":"BUSINESS"},"tasks":[{"title":""}]}`},
		{"bad intimation id", `{"intimation_id":"not-a-uuid","deadline":{"days":5,"counting":"BUSINESS"},"tasks":[]}`},
		{"bad due_date", `{"intimation_id":"018f0000-0000-7000-8000-000000000abc","deadline":{"days":5,"counting":"BUSINESS"},"tasks":[{"title":"x","due_date":"01/02/2024"}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			wr := &recordingWriter{}
			app := newAppWithWriter(&recordingReader{}, wr, "tenant-9")

			status, body := doJSON(t, app, http.MethodPost, "/v1/prazos/confirm", "jwt", tt.body)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body: %s)", status, body)
			}
			if wr.calls != 0 {
				t.Errorf("writer calls = %d, want 0 (rejected at the edge)", wr.calls)
			}
		})
	}
}

// The use case's typed ErrDeadlineNotFound (no prazo for the intimação) surfaces as 404.
func TestHandler_Confirm_NotFound_404(t *testing.T) {
	t.Parallel()

	wr := &recordingWriter{err: ErrDeadlineNotFound}
	app := newAppWithWriter(&recordingReader{}, wr, "tenant-9")

	body := `{"intimation_id":"018f0000-0000-7000-8000-000000000abc","deadline":{"days":5,"counting":"BUSINESS"},"tasks":[]}`
	status, resBody := doJSON(t, app, http.MethodPost, "/v1/prazos/confirm", "jwt", body)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body: %s)", status, resBody)
	}
	if !strings.Contains(resBody, string(apperr.KindNotFound)) {
		t.Errorf("body missing kind %q\ngot: %s", apperr.KindNotFound, resBody)
	}
}

// No bearer token → 401 at the auth boundary; the confirm handler never runs.
func TestHandler_Confirm_NoToken_401(t *testing.T) {
	t.Parallel()

	wr := &recordingWriter{}
	app := newAppWithWriter(&recordingReader{}, wr, "tenant-9")

	status, _ := doJSON(t, app, http.MethodPost, "/v1/prazos/confirm", "",
		`{"intimation_id":"018f0000-0000-7000-8000-000000000abc","deadline":{"days":5,"counting":"BUSINESS"},"tasks":[]}`)
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
	if wr.calls != 0 {
		t.Errorf("writer calls = %d, want 0 (blocked at auth)", wr.calls)
	}
}

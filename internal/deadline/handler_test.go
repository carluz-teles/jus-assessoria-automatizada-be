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
	byProcRes     PrazosByProcessoResult
	gotByProcQ    PrazosByProcessoQuery
	agendaRes     PrazosResult
	gotAgendaQ    PrazosQuery
	byIntimRes    PrazosResult
	gotByIntimTID string
	gotByIntimID  string
	detailView    PrazoDetailView
	detailErr     error
	gotDetailTID  string
	gotDetailID   string

	tasksByProcRes  TasksByProcessoResult
	gotTasksByProcQ TasksByProcessoQuery
	tasksRes        TasksResult
	gotTasksQ       TasksQuery

	taskDetailView   TaskDetailView
	taskDetailErr    error
	gotTaskDetailTID string
	gotTaskDetailID  string
	prazosSummary    PrazosSummary
	tasksSummary     TasksSummary
	gotSummaryTID    string
}

func (r *recordingReader) PrazosByProcesso(_ context.Context, q PrazosByProcessoQuery) (PrazosByProcessoResult, error) {
	r.gotByProcQ = q
	return r.byProcRes, nil
}

func (r *recordingReader) Prazos(_ context.Context, q PrazosQuery) (PrazosResult, error) {
	r.gotAgendaQ = q
	return r.agendaRes, nil
}

func (r *recordingReader) PrazosByIntimacao(_ context.Context, tenantID, intimationID string) (PrazosResult, error) {
	r.gotByIntimTID, r.gotByIntimID = tenantID, intimationID
	return r.byIntimRes, nil
}

func (r *recordingReader) Prazo(_ context.Context, tenantID, id string) (PrazoDetailView, error) {
	r.gotDetailTID, r.gotDetailID = tenantID, id
	return r.detailView, r.detailErr
}

func (r *recordingReader) TasksByProcesso(_ context.Context, q TasksByProcessoQuery) (TasksByProcessoResult, error) {
	r.gotTasksByProcQ = q
	return r.tasksByProcRes, nil
}

func (r *recordingReader) Tasks(_ context.Context, q TasksQuery) (TasksResult, error) {
	r.gotTasksQ = q
	return r.tasksRes, nil
}

func (r *recordingReader) TaskDetail(_ context.Context, tenantID, id string) (TaskDetailView, error) {
	r.gotTaskDetailTID, r.gotTaskDetailID = tenantID, id
	return r.taskDetailView, r.taskDetailErr
}

func (r *recordingReader) PrazosSummary(_ context.Context, tenantID string) (PrazosSummary, error) {
	r.gotSummaryTID = tenantID
	return r.prazosSummary, nil
}

func (r *recordingReader) TasksSummary(_ context.Context, tenantID string) (TasksSummary, error) {
	r.gotSummaryTID = tenantID
	return r.tasksSummary, nil
}

// recordingWriter implements the handler's writer port, capturing the commands the handler
// forwards and returning canned results/errors for each write entry point.
type recordingWriter struct {
	gotCmd ConfirmCommand
	calls  int
	res    ConfirmResult
	err    error

	// adjust
	gotAdjustCmd AdjustCommand
	adjustCalls  int
	adjustRes    AdjustedDeadline
	adjustErr    error

	// met / missed
	gotMetTenant, gotMetID       string
	metCalls                     int
	metRes                       MarkedDeadline
	metErr                       error
	gotMissedTenant, gotMissedID string
	missedCalls                  int
	missedRes                    MarkedDeadline
	missedErr                    error

	// task writes
	gotCreateTaskCmd               CreateTaskCommand
	createTaskCalls                int
	createTaskRes                  *Task
	createTaskErr                  error
	gotUpdateTaskCmd               UpdateTaskCommand
	updateTaskCalls                int
	updateTaskRes                  *Task
	updateTaskErr                  error
	gotDoneTenant, gotDoneID       string
	doneCalls                      int
	doneRes                        TaskTransition
	doneErr                        error
	gotDismissTenant, gotDismissID string
	dismissCalls                   int
	dismissRes                     TaskTransition
	dismissErr                     error

	// task_item writes
	gotCreateItemCmd                                        CreateTaskItemCommand
	createItemCalls                                         int
	createItemRes                                           *TaskItem
	createItemErr                                           error
	gotUpdateItemCmd                                        UpdateTaskItemCommand
	updateItemCalls                                         int
	updateItemRes                                           *TaskItem
	updateItemErr                                           error
	gotDeleteItemTenant, gotDeleteItemTask, gotDeleteItemID string
	deleteItemCalls                                         int
	deleteItemErr                                           error
}

func (w *recordingWriter) Confirm(_ context.Context, cmd ConfirmCommand) (ConfirmResult, error) {
	w.calls++
	w.gotCmd = cmd
	return w.res, w.err
}

func (w *recordingWriter) Adjust(_ context.Context, cmd AdjustCommand) (AdjustedDeadline, error) {
	w.adjustCalls++
	w.gotAdjustCmd = cmd
	return w.adjustRes, w.adjustErr
}

func (w *recordingWriter) MarkMet(_ context.Context, tenantID, deadlineID string) (MarkedDeadline, error) {
	w.metCalls++
	w.gotMetTenant, w.gotMetID = tenantID, deadlineID
	return w.metRes, w.metErr
}

func (w *recordingWriter) MarkMissed(_ context.Context, tenantID, deadlineID string) (MarkedDeadline, error) {
	w.missedCalls++
	w.gotMissedTenant, w.gotMissedID = tenantID, deadlineID
	return w.missedRes, w.missedErr
}

func (w *recordingWriter) CreateTask(_ context.Context, cmd CreateTaskCommand) (*Task, error) {
	w.createTaskCalls++
	w.gotCreateTaskCmd = cmd
	return w.createTaskRes, w.createTaskErr
}

func (w *recordingWriter) UpdateTask(_ context.Context, cmd UpdateTaskCommand) (*Task, error) {
	w.updateTaskCalls++
	w.gotUpdateTaskCmd = cmd
	return w.updateTaskRes, w.updateTaskErr
}

func (w *recordingWriter) MarkTaskDone(_ context.Context, tenantID, taskID string) (TaskTransition, error) {
	w.doneCalls++
	w.gotDoneTenant, w.gotDoneID = tenantID, taskID
	return w.doneRes, w.doneErr
}

func (w *recordingWriter) DismissTask(_ context.Context, tenantID, taskID string) (TaskTransition, error) {
	w.dismissCalls++
	w.gotDismissTenant, w.gotDismissID = tenantID, taskID
	return w.dismissRes, w.dismissErr
}

func (w *recordingWriter) CreateTaskItem(_ context.Context, cmd CreateTaskItemCommand) (*TaskItem, error) {
	w.createItemCalls++
	w.gotCreateItemCmd = cmd
	return w.createItemRes, w.createItemErr
}

func (w *recordingWriter) UpdateTaskItem(_ context.Context, cmd UpdateTaskItemCommand) (*TaskItem, error) {
	w.updateItemCalls++
	w.gotUpdateItemCmd = cmd
	return w.updateItemRes, w.updateItemErr
}

func (w *recordingWriter) DeleteTaskItem(_ context.Context, tenantID, taskID, itemID string) error {
	w.deleteItemCalls++
	w.gotDeleteItemTenant, w.gotDeleteItemTask, w.gotDeleteItemID = tenantID, taskID, itemID
	return w.deleteItemErr
}

// recordingSuggester implements the handler's suggester port, returning a canned Suggestion
// (or error) so the endpoint's DTO mapping can be asserted without an LLM.
type recordingSuggester struct {
	res Suggestion
	err error
}

func (s recordingSuggester) SuggestTasks(context.Context, string, string) (Suggestion, error) {
	return s.res, s.err
}

// newApp builds an app whose /v1 group mirrors production: Auth resolves a principal with
// the given tenant, then the deadline routes mount under it. It uses a throwaway writer —
// the read tests never hit the confirm route; newAppWithWriter injects a specific one.
func newApp(rd reader, tenant string) *fiber.App {
	return newAppWithWriter(rd, &recordingWriter{}, tenant)
}

// newAppWithWriter is newApp with an explicit writer, for the confirm-route tests.
func newAppWithWriter(rd reader, wr writer, tenant string) *fiber.App {
	return newAppWith(rd, wr, nil, tenant)
}

// newAppWithSuggester is newApp with an explicit suggester, for the suggested-tasks tests. A nil
// suggester exercises the LLM-unconfigured degradation.
func newAppWithSuggester(rd reader, sg suggester, tenant string) *fiber.App {
	return newAppWith(rd, &recordingWriter{}, sg, tenant)
}

// newAppWith wires the handler with the given ports under the Auth boundary.
func newAppWith(rd reader, wr writer, sg suggester, tenant string) *fiber.App {
	app := fiber.New(fiber.Config{
		ErrorHandler: func(c *fiber.Ctx, err error) error { return httpx.WriteError(c, err) },
	})
	v1 := app.Group("/v1", middleware.Auth(stubVerifier{}, stubResolver{tenant: tenant}))
	NewHandler(rd, wr, sg).RegisterV1(v1)
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

// ?kind/?court (the envelope's free-text options) flow into the PrazosQuery the handler
// sends to the read port.
func TestHandler_ListPrazos_ForwardsKindAndCourt(t *testing.T) {
	t.Parallel()

	rd := &recordingReader{}
	app := newApp(rd, "tenant-9")

	status, _ := do(t, app, http.MethodGet,
		"/v1/prazos?kind=Aguardando%20resposta&court=TJSP", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if rd.gotAgendaQ.Kind != "Aguardando resposta" {
		t.Errorf("Kind = %q, want Aguardando resposta", rd.gotAgendaQ.Kind)
	}
	if rd.gotAgendaQ.Court != "TJSP" {
		t.Errorf("Court = %q, want TJSP", rd.gotAgendaQ.Court)
	}
}

// A param outside the prazos route's allowlist is a client error → 400, never silently
// ignored (docs/erd-backend.md §4e.3).
func TestHandler_ListPrazos_UnknownParam_400(t *testing.T) {
	t.Parallel()

	app := newApp(&recordingReader{}, "tenant-9")
	status, _ := do(t, app, http.MethodGet, "/v1/prazos?assignee=u-1", "jwt")
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a foreign param", status)
	}
}

// The prazos envelope's filters block is always present and never null.
func TestHandler_ListPrazos_EnvelopeFiltersAlwaysObject(t *testing.T) {
	t.Parallel()

	app := newApp(&recordingReader{}, "tenant-9")
	status, body := do(t, app, http.MethodGet, "/v1/prazos", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.Contains(body, `"filters":{}`) {
		t.Errorf("envelope filters not an empty object\ngot: %s", body)
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

// --- GET /v1/prazos?intimation_id=... (F2 lookup) ---------------------------

// With ?intimation_id set the handler takes the by-intimação path: it forwards the id
// and the principal's tenant (never the query) and returns the prazo in the same envelope.
func TestHandler_ListPrazos_ByIntimacao_ReturnsPrazo(t *testing.T) {
	t.Parallel()

	rd := &recordingReader{byIntimRes: PrazosResult{
		Items: []AgendaPrazoView{{
			ID: "d-1", EndDate: time.Date(2024, 3, 11, 0, 0, 0, 0, time.UTC),
			Status: "PENDING", IntimationID: "018f0000-0000-7000-8000-000000000abc",
			CNJNumber: "0001", Court: "TJSP", CourtRecordID: "cr-1",
		}},
		TotalCount: 1,
		Total:      1,
	}}
	app := newApp(rd, "tenant-9")

	status, body := do(t, app, http.MethodGet,
		"/v1/prazos?intimation_id=018f0000-0000-7000-8000-000000000abc", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", status, body)
	}
	if rd.gotByIntimID != "018f0000-0000-7000-8000-000000000abc" {
		t.Errorf("forwarded intimation id = %q, want the query id", rd.gotByIntimID)
	}
	if rd.gotByIntimTID != "tenant-9" {
		t.Errorf("TenantID = %q, want tenant-9 (from principal)", rd.gotByIntimTID)
	}
	// The agenda path must NOT run when ?intimation_id is present.
	if rd.gotAgendaQ.TenantID != "" {
		t.Errorf("agenda path ran (query %+v), want by-intimação only", rd.gotAgendaQ)
	}
	for _, want := range []string{
		`"intimation_id":"018f0000-0000-7000-8000-000000000abc"`, `"total":1`, `"court":"TJSP"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %s\ngot: %s", want, body)
		}
	}
}

// An intimação with no derived prazo serializes as an empty data array (never null) with
// zero totals — 200, so the F2 screen renders "sem prazo".
func TestHandler_ListPrazos_ByIntimacao_NoPrazo_EmptyData(t *testing.T) {
	t.Parallel()

	app := newApp(&recordingReader{}, "tenant-9")
	status, body := do(t, app, http.MethodGet,
		"/v1/prazos?intimation_id=018f0000-0000-7000-8000-000000000abc", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	for _, want := range []string{`"data":[]`, `"next_cursor":null`, `"total":0`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %s\ngot: %s", want, body)
		}
	}
}

// A malformed ?intimation_id (not a uuid) is a client error → 400, and the read port is
// never called.
func TestHandler_ListPrazos_ByIntimacao_BadID_400(t *testing.T) {
	t.Parallel()

	rd := &recordingReader{}
	app := newApp(rd, "tenant-9")
	status, _ := do(t, app, http.MethodGet, "/v1/prazos?intimation_id=not-a-uuid", "jwt")
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
	if rd.gotByIntimID != "" {
		t.Errorf("read port called with %q, want no call on a bad id", rd.gotByIntimID)
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

// --- GET /v1/prazos/:id/suggested-tasks --------------------------------------

// The endpoint returns the single Suggestion object: summary + recommendation at the top and
// each task carrying its description (the v2 contract, one LLM call, no list envelope).
func TestHandler_SuggestedTasks_ReturnsSummaryRecommendationAndDescriptions(t *testing.T) {
	t.Parallel()

	sg := recordingSuggester{res: Suggestion{
		Summary:        "O réu foi citado para contestar.",
		Recommendation: "Elaborar e protocolar a contestação em 15 dias úteis.",
		Tasks: []SuggestedTask{
			{Title: "Redigir contestação", Kind: "PECA", Description: "Elaborar a peça de defesa."},
		},
	}}
	app := newAppWithSuggester(&recordingReader{}, sg, "tenant-9")

	status, body := do(t, app, http.MethodGet, "/v1/prazos/d-1/suggested-tasks", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", status, body)
	}
	for _, want := range []string{
		`"summary":"O réu foi citado para contestar."`,
		`"recommendation":"Elaborar e protocolar a contestação em 15 dias úteis."`,
		`"description":"Elaborar a peça de defesa."`,
		`"title":"Redigir contestação"`,
		`"kind":"PECA"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %s\ngot: %s", want, body)
		}
	}
}

// With no suggester (LLM unconfigured) the endpoint still answers 200 with empty strings and an
// empty (non-null) list — the form degrades gracefully.
func TestHandler_SuggestedTasks_NoSuggester_Empty(t *testing.T) {
	t.Parallel()

	app := newApp(&recordingReader{}, "tenant-9")

	status, body := do(t, app, http.MethodGet, "/v1/prazos/d-1/suggested-tasks", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", status, body)
	}
	for _, want := range []string{`"summary":""`, `"recommendation":""`, `"suggested_tasks":[]`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %s\ngot: %s", want, body)
		}
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
		"deadline":{"kind":"CONTESTACAO","days":15,"counting":"BUSINESS","doubled":false}}`
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
	if !strings.Contains(resBody, `"id":"d-1"`) || !strings.Contains(resBody, `"status":"OPEN"`) {
		t.Errorf("response missing confirmed deadline\ngot: %s", resBody)
	}
}

// A malformed body (bad counting, non-positive days, bad uuid) is a 400 with the {kind,...}
// envelope, and the use case is never called. Task validation is no longer part of the confirm
// (tasks moved to POST /v1/tasks), so the body carries no tasks.
func TestHandler_Confirm_Validation_400(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
	}{
		{"bad counting", `{"intimation_id":"018f0000-0000-7000-8000-000000000abc","deadline":{"days":15,"counting":"WEEKLY"}}`},
		{"days zero", `{"intimation_id":"018f0000-0000-7000-8000-000000000abc","deadline":{"days":0,"counting":"BUSINESS"}}`},
		{"bad intimation id", `{"intimation_id":"not-a-uuid","deadline":{"days":5,"counting":"BUSINESS"}}`},
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

// --- PATCH /v1/prazos/:id (ajuste) ------------------------------------------

// The ajuste handler takes tenant_id/confirmed_by from the PRINCIPAL and the prazo id from the
// PATH (never the body), forwards ONLY the present fields (a partial patch), and returns 200
// with the recomputed prazo.
func TestHandler_Adjust_ForwardsPartialPatchFromPrincipalAndPath(t *testing.T) {
	t.Parallel()

	wr := &recordingWriter{adjustRes: AdjustedDeadline{ID: "d-1", Days: 10, Status: StatusOpen, Counting: CountingBusiness}}
	app := newAppWithWriter(&recordingReader{}, wr, "tenant-9")

	// Only days + counting present; kind/doubled/doubled_reason are absent (nil).
	body := `{"days":10,"counting":"BUSINESS"}`
	status, resBody := doJSON(t, app, http.MethodPatch, "/v1/prazos/d-1", "jwt", body)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", status, resBody)
	}
	if wr.adjustCalls != 1 {
		t.Fatalf("adjust calls = %d, want 1", wr.adjustCalls)
	}
	cmd := wr.gotAdjustCmd
	if cmd.TenantID != "tenant-9" || cmd.UserID != "u-1" || cmd.DeadlineID != "d-1" {
		t.Errorf("tenant/user/id = %q/%q/%q, want tenant-9/u-1/d-1", cmd.TenantID, cmd.UserID, cmd.DeadlineID)
	}
	if cmd.Days == nil || *cmd.Days != 10 || cmd.Counting == nil || *cmd.Counting != CountingBusiness {
		t.Errorf("present fields days/counting = %v/%v, want 10/BUSINESS", cmd.Days, cmd.Counting)
	}
	if cmd.Kind != nil || cmd.Doubled != nil || cmd.DoubledReason != nil {
		t.Errorf("absent fields carried non-nil: kind=%v doubled=%v reason=%v", cmd.Kind, cmd.Doubled, cmd.DoubledReason)
	}
	if !strings.Contains(resBody, `"id":"d-1"`) || !strings.Contains(resBody, `"days":10`) || !strings.Contains(resBody, `"status":"OPEN"`) {
		t.Errorf("response missing recomputed deadline\ngot: %s", resBody)
	}
}

// A present-but-invalid field (days ≤ 0 or a bad counting) is a 400; the use case is never
// called. An absent field is fine (nothing to validate).
func TestHandler_Adjust_Validation_400(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
	}{
		{"days zero", `{"days":0}`},
		{"days negative", `{"days":-3}`},
		{"bad counting", `{"counting":"WEEKLY"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			wr := &recordingWriter{}
			app := newAppWithWriter(&recordingReader{}, wr, "tenant-9")

			status, body := doJSON(t, app, http.MethodPatch, "/v1/prazos/d-1", "jwt", tt.body)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body: %s)", status, body)
			}
			if wr.adjustCalls != 0 {
				t.Errorf("adjust calls = %d, want 0 (rejected at the edge)", wr.adjustCalls)
			}
		})
	}
}

// A terminal prazo is the use case's typed ErrDeadlineNotAdjustable → 409; a miss → 404.
func TestHandler_Adjust_StatusErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantKind   string
	}{
		{"terminal → 409", ErrDeadlineNotAdjustable, http.StatusConflict, string(apperr.KindConflict)},
		{"missing → 404", ErrDeadlineNotFound, http.StatusNotFound, string(apperr.KindNotFound)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			wr := &recordingWriter{adjustErr: tt.err}
			app := newAppWithWriter(&recordingReader{}, wr, "tenant-9")

			status, body := doJSON(t, app, http.MethodPatch, "/v1/prazos/d-1", "jwt", `{"days":10}`)
			if status != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body: %s)", status, tt.wantStatus, body)
			}
			if !strings.Contains(body, tt.wantKind) {
				t.Errorf("body missing kind %q\ngot: %s", tt.wantKind, body)
			}
		})
	}
}

// --- POST /v1/prazos/:id/met | .../missed -----------------------------------

// The met/missed handlers take the prazo id from the PATH and tenant from the PRINCIPAL, and
// return 200 with {deadline_id, status}. No body is required.
func TestHandler_MarkMetAndMissed_ForwardsFromPrincipalAndPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		path       string
		wantStatus string
	}{
		{"met", "/v1/prazos/d-1/met", "MET"},
		{"missed", "/v1/prazos/d-1/missed", "MISSED"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			wr := &recordingWriter{
				metRes:    MarkedDeadline{ID: "d-1", Status: StatusMet},
				missedRes: MarkedDeadline{ID: "d-1", Status: StatusMissed},
			}
			app := newAppWithWriter(&recordingReader{}, wr, "tenant-9")

			status, body := doJSON(t, app, http.MethodPost, tt.path, "jwt", "")
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body: %s)", status, body)
			}
			if !strings.Contains(body, `"deadline_id":"d-1"`) || !strings.Contains(body, `"status":"`+tt.wantStatus+`"`) {
				t.Errorf("response = %s, want deadline_id d-1 + status %s", body, tt.wantStatus)
			}
			if tt.name == "met" {
				if wr.metCalls != 1 || wr.gotMetTenant != "tenant-9" || wr.gotMetID != "d-1" {
					t.Errorf("met forwarded calls/tenant/id = %d/%q/%q, want 1/tenant-9/d-1", wr.metCalls, wr.gotMetTenant, wr.gotMetID)
				}
			} else {
				if wr.missedCalls != 1 || wr.gotMissedTenant != "tenant-9" || wr.gotMissedID != "d-1" {
					t.Errorf("missed forwarded calls/tenant/id = %d/%q/%q, want 1/tenant-9/d-1", wr.missedCalls, wr.gotMissedTenant, wr.gotMissedID)
				}
			}
		})
	}
}

// A non-OPEN prazo is the use case's typed ErrDeadlineNotOpen → 409; a miss → 404.
func TestHandler_MarkMet_StatusErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		err        error
		wantStatus int
	}{
		{"not open → 409", ErrDeadlineNotOpen, http.StatusConflict},
		{"missing → 404", ErrDeadlineNotFound, http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			wr := &recordingWriter{metErr: tt.err}
			app := newAppWithWriter(&recordingReader{}, wr, "tenant-9")

			status, body := doJSON(t, app, http.MethodPost, "/v1/prazos/d-1/met", "jwt", "")
			if status != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body: %s)", status, tt.wantStatus, body)
			}
		})
	}
}

// No bearer token → 401 at the auth boundary; the met handler never runs.
func TestHandler_MarkMet_NoToken_401(t *testing.T) {
	t.Parallel()

	wr := &recordingWriter{}
	app := newAppWithWriter(&recordingReader{}, wr, "tenant-9")

	status, _ := doJSON(t, app, http.MethodPost, "/v1/prazos/d-1/met", "", "")
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
	if wr.metCalls != 0 {
		t.Errorf("met calls = %d, want 0 (blocked at auth)", wr.metCalls)
	}
}

// --- GET /v1/processos/:id/tasks --------------------------------------------

// The handler forwards the path :id (court_record) and the decoded ?cursor to the read port,
// clamps ?limit, takes the tenant from the principal, and serializes the row + envelope.
func TestHandler_ListTasksByProcesso_ForwardsAndSerializes(t *testing.T) {
	t.Parallel()

	due := time.Date(2024, 3, 15, 0, 0, 0, 0, time.UTC)
	rd := &recordingReader{tasksByProcRes: TasksByProcessoResult{
		Items: []TaskView{{
			ID: "018f0000-0000-7000-8000-000000000abc", Title: "Contestar", Kind: "PECA",
			DueDate: &due, Status: "OPEN", Source: "MANUAL", DeadlineID: "d-1", CourtRecordID: "cr-77",
		}},
		Total: 1,
	}}
	app := newApp(rd, "tenant-9")

	status, body := do(t, app, http.MethodGet, "/v1/processos/cr-77/tasks?limit=25", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", status, body)
	}
	if rd.gotTasksByProcQ.CourtRecordID != "cr-77" || rd.gotTasksByProcQ.TenantID != "tenant-9" {
		t.Errorf("forwarded cr/tenant = %q/%q, want cr-77/tenant-9", rd.gotTasksByProcQ.CourtRecordID, rd.gotTasksByProcQ.TenantID)
	}
	if rd.gotTasksByProcQ.Limit != 25 {
		t.Errorf("Limit = %d, want 25", rd.gotTasksByProcQ.Limit)
	}
	if rd.gotTasksByProcQ.LastDue != minDate || rd.gotTasksByProcQ.LastID != zeroUUID {
		t.Errorf("first-page sentinel = (%q, %q), want (%q, %q)", rd.gotTasksByProcQ.LastDue, rd.gotTasksByProcQ.LastID, minDate, zeroUUID)
	}
	for _, want := range []string{`"title":"Contestar"`, `"status":"OPEN"`, `"source":"MANUAL"`, `"deadline_id":"d-1"`, `"total":1`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %s\ngot: %s", want, body)
		}
	}
}

// --- GET /v1/tasks (agenda) --------------------------------------------------

// The handler forwards ?status and the ?from/?to window (validated) to the read port; the tenant
// comes from the principal.
func TestHandler_ListTasks_ForwardsFilters(t *testing.T) {
	t.Parallel()

	rd := &recordingReader{}
	app := newApp(rd, "tenant-9")

	status, _ := do(t, app, http.MethodGet, "/v1/tasks?status=OPEN&from=2024-03-01&to=2024-03-31&limit=10", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if rd.gotTasksQ.Status != "OPEN" || rd.gotTasksQ.From != "2024-03-01" || rd.gotTasksQ.To != "2024-03-31" {
		t.Errorf("filters not forwarded: %+v", rd.gotTasksQ)
	}
	if rd.gotTasksQ.TenantID != "tenant-9" {
		t.Errorf("TenantID = %q, want tenant-9 (from principal)", rd.gotTasksQ.TenantID)
	}
}

// ?assignee=me resolves to the principal's own user id (the "meus prazos" shortcut) — the tenant
// and the resolved assignee both come from the verified principal, never the query.
func TestHandler_ListTasks_AssigneeMeResolvesToPrincipal(t *testing.T) {
	t.Parallel()

	rd := &recordingReader{}
	app := newApp(rd, "tenant-9")

	status, _ := do(t, app, http.MethodGet, "/v1/tasks?assignee=me", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	// stubResolver hands the handler UserID "u-1".
	if rd.gotTasksQ.Assignee != "u-1" {
		t.Errorf("Assignee = %q, want u-1 (principal's id via \"me\")", rd.gotTasksQ.Assignee)
	}
}

// An unknown ?status and a malformed ?assignee are client errors → 400 (the read port is never called).
func TestHandler_ListTasks_BadFilters_400(t *testing.T) {
	t.Parallel()

	tests := []struct{ name, query string }{
		{"bad status", "/v1/tasks?status=BOGUS"},
		{"bad assignee", "/v1/tasks?assignee=not-a-uuid"},
		{"bad date", "/v1/tasks?from=03-2024"},
		{"bad source", "/v1/tasks?source=LLM"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			app := newApp(&recordingReader{}, "tenant-9")
			status, body := do(t, app, http.MethodGet, tt.query, "jwt")
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body: %s)", status, body)
			}
		})
	}
}

// ?source (a closed set) flows into the TasksQuery the handler sends to the read port.
func TestHandler_ListTasks_ForwardsSource(t *testing.T) {
	t.Parallel()

	rd := &recordingReader{}
	app := newApp(rd, "tenant-9")

	status, _ := do(t, app, http.MethodGet, "/v1/tasks?source=AI", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if rd.gotTasksQ.Source != "AI" {
		t.Errorf("Source = %q, want AI", rd.gotTasksQ.Source)
	}
}

// A param outside the tasks route's allowlist is a client error → 400, never silently
// ignored.
func TestHandler_ListTasks_UnknownParam_400(t *testing.T) {
	t.Parallel()

	app := newApp(&recordingReader{}, "tenant-9")
	status, _ := do(t, app, http.MethodGet, "/v1/tasks?court=TJSP", "jwt")
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a foreign param", status)
	}
}

// The tasks envelope's filters block is always present and never null.
func TestHandler_ListTasks_EnvelopeFiltersAlwaysObject(t *testing.T) {
	t.Parallel()

	app := newApp(&recordingReader{}, "tenant-9")
	status, body := do(t, app, http.MethodGet, "/v1/tasks", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.Contains(body, `"filters":{}`) {
		t.Errorf("envelope filters not an empty object\ngot: %s", body)
	}
}

// An empty agenda serializes as an empty data array (never null) with zero totals — 200.
func TestHandler_ListTasks_Empty(t *testing.T) {
	t.Parallel()

	app := newApp(&recordingReader{}, "tenant-9")
	status, body := do(t, app, http.MethodGet, "/v1/tasks", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	for _, want := range []string{`"data":[]`, `"next_cursor":null`, `"total":0`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %s\ngot: %s", want, body)
		}
	}
}

// Cursor round-trip across two pages: an undated last row keys its next cursor off the
// '9999-12-31' sentinel (sortDue), and echoing it back resumes the keyset exactly there.
func TestHandler_ListTasks_CursorRoundTrip_UndatedSentinel(t *testing.T) {
	t.Parallel()

	last := TaskView{ID: "018f0000-0000-7000-8000-0000000000ff"} // undated → sortDue is the zero time...
	last.sortDue = time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC) // ...set to the coalesced sentinel by the repo
	rd := &recordingReader{tasksRes: TasksResult{
		Items:   []TaskView{{ID: "first"}, last},
		HasMore: true,
		Total:   50,
	}}
	app := newApp(rd, "tenant-9")

	status, body := do(t, app, http.MethodGet, "/v1/tasks?limit=2", "jwt")
	if status != http.StatusOK {
		t.Fatalf("page 1 status = %d, want 200", status)
	}
	var page1 httpx.Page[TaskView]
	if err := json.Unmarshal([]byte(body), &page1); err != nil {
		t.Fatalf("unmarshal page 1: %v (body: %s)", err, body)
	}
	if page1.Page.NextCursor == nil {
		t.Fatal("page 1 next_cursor = nil, want a token (HasMore was true)")
	}

	status, _ = do(t, app, http.MethodGet, "/v1/tasks?limit=2&cursor="+*page1.Page.NextCursor, "jwt")
	if status != http.StatusOK {
		t.Fatalf("page 2 status = %d, want 200", status)
	}
	if rd.gotTasksQ.LastDue != "9999-12-31" || rd.gotTasksQ.LastID != last.ID {
		t.Errorf("page 2 keyset = (%q, %q), want (9999-12-31, %q)", rd.gotTasksQ.LastDue, rd.gotTasksQ.LastID, last.ID)
	}
}

// --- POST /v1/tasks ----------------------------------------------------------

// The create handler takes tenant_id/created_by from the PRINCIPAL (never the body), forwards the
// mapped command, and returns 201 with the created task in the shared TaskView shape.
func TestHandler_CreateTask_ForwardsCommandFromPrincipal_201(t *testing.T) {
	t.Parallel()

	wr := &recordingWriter{createTaskRes: &Task{ID: "t-1", Title: "Peça", Status: TaskStatusOpen, Source: SourceManual}}
	app := newAppWithWriter(&recordingReader{}, wr, "tenant-9")

	body := `{"deadline_id":"018f0000-0000-7000-8000-000000000abc","title":"Peça","due_date":"2024-03-15",
		"assignee_user_id":"018f0000-0000-7000-8000-000000000def"}`
	status, resBody := doJSON(t, app, http.MethodPost, "/v1/tasks", "jwt", body)
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body: %s)", status, resBody)
	}
	if wr.createTaskCalls != 1 {
		t.Fatalf("create calls = %d, want 1", wr.createTaskCalls)
	}
	cmd := wr.gotCreateTaskCmd
	if cmd.TenantID != "tenant-9" || cmd.UserID != "u-1" {
		t.Errorf("tenant/user = %q/%q, want tenant-9/u-1 (from principal)", cmd.TenantID, cmd.UserID)
	}
	if cmd.Title != "Peça" || cmd.DueDate == nil || cmd.DeadlineID != "018f0000-0000-7000-8000-000000000abc" {
		t.Errorf("command = %+v", cmd)
	}
	if !strings.Contains(resBody, `"id":"t-1"`) || !strings.Contains(resBody, `"status":"OPEN"`) || !strings.Contains(resBody, `"source":"MANUAL"`) {
		t.Errorf("response missing created task\ngot: %s", resBody)
	}
}

// A malformed body (empty title, bad due_date, bad optional uuid) is a 400, and the use case is
// never called.
func TestHandler_CreateTask_Validation_400(t *testing.T) {
	t.Parallel()

	tests := []struct{ name, body string }{
		{"empty title", `{"title":""}`},
		{"missing title", `{"description":"x"}`},
		{"bad due_date", `{"title":"x","due_date":"01/02/2024"}`},
		{"bad deadline id", `{"title":"x","deadline_id":"not-a-uuid"}`},
		{"bad kind", `{"title":"x","kind":"OUTRO_ENUM"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			wr := &recordingWriter{}
			app := newAppWithWriter(&recordingReader{}, wr, "tenant-9")

			status, body := doJSON(t, app, http.MethodPost, "/v1/tasks", "jwt", tt.body)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body: %s)", status, body)
			}
			if wr.createTaskCalls != 0 {
				t.Errorf("create calls = %d, want 0 (rejected at the edge)", wr.createTaskCalls)
			}
		})
	}
}

// --- PATCH /v1/tasks/:id -----------------------------------------------------

// The patch handler takes tenant from the PRINCIPAL and the task id from the PATH (never the body),
// forwards ONLY the present fields (a partial patch), and returns 200 with the saved task.
func TestHandler_UpdateTask_ForwardsPartialPatch(t *testing.T) {
	t.Parallel()

	wr := &recordingWriter{updateTaskRes: &Task{ID: "t-1", Title: "novo", Status: TaskStatusOpen, Source: SourceManual}}
	app := newAppWithWriter(&recordingReader{}, wr, "tenant-9")

	body := `{"title":"novo","due_date":""}` // title set, due_date cleared; kind/description/assignee absent
	status, resBody := doJSON(t, app, http.MethodPatch, "/v1/tasks/t-1", "jwt", body)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", status, resBody)
	}
	if wr.updateTaskCalls != 1 {
		t.Fatalf("update calls = %d, want 1", wr.updateTaskCalls)
	}
	cmd := wr.gotUpdateTaskCmd
	if cmd.TenantID != "tenant-9" || cmd.TaskID != "t-1" {
		t.Errorf("tenant/id = %q/%q, want tenant-9/t-1", cmd.TenantID, cmd.TaskID)
	}
	if cmd.Title == nil || *cmd.Title != "novo" || cmd.DueDate == nil || *cmd.DueDate != "" {
		t.Errorf("present fields title/due = %v/%v, want novo/\"\" (cleared)", cmd.Title, cmd.DueDate)
	}
	if cmd.Kind != nil || cmd.Description != nil || cmd.AssigneeUserID != nil {
		t.Errorf("absent fields carried non-nil: kind=%v desc=%v assignee=%v", cmd.Kind, cmd.Description, cmd.AssigneeUserID)
	}
	if !strings.Contains(resBody, `"id":"t-1"`) || !strings.Contains(resBody, `"title":"novo"`) {
		t.Errorf("response missing saved task\ngot: %s", resBody)
	}
}

// A present-but-blank title is a 400; the use case is never called.
func TestHandler_UpdateTask_Validation_400(t *testing.T) {
	t.Parallel()

	tests := []struct{ name, body string }{
		{"blank title", `{"title":""}`},
		{"bad due_date", `{"due_date":"2024/03/15"}`},
		{"bad assignee", `{"assignee_user_id":"nope"}`},
		{"bad kind", `{"kind":"OUTRO_ENUM"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			wr := &recordingWriter{}
			app := newAppWithWriter(&recordingReader{}, wr, "tenant-9")

			status, body := doJSON(t, app, http.MethodPatch, "/v1/tasks/t-1", "jwt", tt.body)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body: %s)", status, body)
			}
			if wr.updateTaskCalls != 0 {
				t.Errorf("update calls = %d, want 0 (rejected at the edge)", wr.updateTaskCalls)
			}
		})
	}
}

// A miss is the use case's typed ErrTaskNotFound → 404.
func TestHandler_UpdateTask_NotFound_404(t *testing.T) {
	t.Parallel()

	wr := &recordingWriter{updateTaskErr: ErrTaskNotFound}
	app := newAppWithWriter(&recordingReader{}, wr, "tenant-9")

	status, body := doJSON(t, app, http.MethodPatch, "/v1/tasks/missing", "jwt", `{"title":"x"}`)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body: %s)", status, body)
	}
	if !strings.Contains(body, string(apperr.KindNotFound)) {
		t.Errorf("body missing kind %q\ngot: %s", apperr.KindNotFound, body)
	}
}

// --- POST /v1/tasks/:id/done | .../dismiss ----------------------------------

// The done/dismiss handlers take the task id from the PATH and tenant from the PRINCIPAL, and
// return 200 with {task_id, status}. No body is required.
func TestHandler_TaskDoneAndDismiss_ForwardsFromPrincipalAndPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, path, wantStatus string
	}{
		{"done", "/v1/tasks/t-1/done", "DONE"},
		{"dismiss", "/v1/tasks/t-1/dismiss", "DISMISSED"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			wr := &recordingWriter{
				doneRes:    TaskTransition{ID: "t-1", Status: TaskStatusDone},
				dismissRes: TaskTransition{ID: "t-1", Status: TaskStatusDismissed},
			}
			app := newAppWithWriter(&recordingReader{}, wr, "tenant-9")

			status, body := doJSON(t, app, http.MethodPost, tt.path, "jwt", "")
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body: %s)", status, body)
			}
			if !strings.Contains(body, `"task_id":"t-1"`) || !strings.Contains(body, `"status":"`+tt.wantStatus+`"`) {
				t.Errorf("response = %s, want task_id t-1 + status %s", body, tt.wantStatus)
			}
			if tt.name == "done" {
				if wr.doneCalls != 1 || wr.gotDoneTenant != "tenant-9" || wr.gotDoneID != "t-1" {
					t.Errorf("done forwarded calls/tenant/id = %d/%q/%q, want 1/tenant-9/t-1", wr.doneCalls, wr.gotDoneTenant, wr.gotDoneID)
				}
			} else {
				if wr.dismissCalls != 1 || wr.gotDismissTenant != "tenant-9" || wr.gotDismissID != "t-1" {
					t.Errorf("dismiss forwarded calls/tenant/id = %d/%q/%q, want 1/tenant-9/t-1", wr.dismissCalls, wr.gotDismissTenant, wr.gotDismissID)
				}
			}
		})
	}
}

// A non-OPEN task is the use case's typed ErrTaskNotOpen → 409; a miss → 404.
func TestHandler_TaskDone_StatusErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		err        error
		wantStatus int
	}{
		{"not open → 409", ErrTaskNotOpen, http.StatusConflict},
		{"missing → 404", ErrTaskNotFound, http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			wr := &recordingWriter{doneErr: tt.err}
			app := newAppWithWriter(&recordingReader{}, wr, "tenant-9")

			status, body := doJSON(t, app, http.MethodPost, "/v1/tasks/t-1/done", "jwt", "")
			if status != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body: %s)", status, tt.wantStatus, body)
			}
		})
	}
}

// --- GET /v1/tasks/:id (detail) ---------------------------------------------

// The detail endpoint returns the task fields + checklist + progress + display_status, and
// forwards the tenant from the principal.
func TestHandler_GetTask_OK(t *testing.T) {
	t.Parallel()

	rd := &recordingReader{taskDetailView: TaskDetailView{
		ID: "t-1", Title: "Contestar", Status: "OPEN", DisplayStatus: "Em execução",
		Items:    []TaskItemView{{ID: "i-1", Title: "Ler", Done: true}},
		Progress: TaskProgress{Done: 1, Total: 2},
	}}
	app := newApp(rd, "tenant-9")

	status, body := do(t, app, http.MethodGet, "/v1/tasks/t-1", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", status, body)
	}
	if rd.gotTaskDetailTID != "tenant-9" || rd.gotTaskDetailID != "t-1" {
		t.Errorf("forwarded (tenant,id) = (%q,%q), want (tenant-9,t-1)", rd.gotTaskDetailTID, rd.gotTaskDetailID)
	}
	for _, want := range []string{`"display_status":"Em execução"`, `"progress":{"done":1,"total":2}`, `"items":[`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %s\ngot: %s", want, body)
		}
	}
}

// A miss is the repo's typed ErrTaskNotFound → 404 with the {kind,...} envelope.
func TestHandler_GetTask_NotFound_404(t *testing.T) {
	t.Parallel()

	rd := &recordingReader{taskDetailErr: ErrTaskNotFound}
	app := newApp(rd, "tenant-9")

	status, body := do(t, app, http.MethodGet, "/v1/tasks/missing", "jwt")
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body: %s)", status, body)
	}
	if !strings.Contains(body, string(apperr.KindNotFound)) {
		t.Errorf("body missing kind %q\ngot: %s", apperr.KindNotFound, body)
	}
}

// --- GET /v1/prazos/summary | /v1/tasks/summary -----------------------------

// The summary routes are static and win over the :id param routes; they return the KPI buckets
// and forward the tenant from the principal.
func TestHandler_Summaries_ReturnBucketsForTenant(t *testing.T) {
	t.Parallel()

	rd := &recordingReader{
		prazosSummary: PrazosSummary{Total: 10, Criticos: 2, Vencendo: 3, Abertos: 6, Futuros: 1, Vencidos: 2, Cumpridos: 2},
		tasksSummary:  TasksSummary{Abertas: 4, EmExecucao: 3, Concluidas: 5, Atrasadas: 1},
	}
	app := newApp(rd, "tenant-9")

	status, body := do(t, app, http.MethodGet, "/v1/prazos/summary", "jwt")
	if status != http.StatusOK {
		t.Fatalf("prazos/summary status = %d, want 200 (body: %s)", status, body)
	}
	if rd.gotSummaryTID != "tenant-9" {
		t.Errorf("prazos summary tenant = %q, want tenant-9", rd.gotSummaryTID)
	}
	for _, want := range []string{`"total":10`, `"criticos":2`, `"vencendo":3`, `"cumpridos":2`} {
		if !strings.Contains(body, want) {
			t.Errorf("prazos/summary missing %s\ngot: %s", want, body)
		}
	}

	status, body = do(t, app, http.MethodGet, "/v1/tasks/summary", "jwt")
	if status != http.StatusOK {
		t.Fatalf("tasks/summary status = %d, want 200 (body: %s)", status, body)
	}
	for _, want := range []string{`"abertas":4`, `"em_execucao":3`, `"concluidas":5`, `"atrasadas":1`} {
		if !strings.Contains(body, want) {
			t.Errorf("tasks/summary missing %s\ngot: %s", want, body)
		}
	}
}

// --- POST /v1/tasks/:id/items -----------------------------------------------

// The create handler takes tenant from the PRINCIPAL and the task id from the PATH (never the body),
// forwards the title, and returns 201 with the created item.
func TestHandler_CreateTaskItem_ForwardsFromPrincipalAndPath_201(t *testing.T) {
	t.Parallel()

	created := time.Date(2024, 3, 20, 9, 0, 0, 0, time.UTC)
	wr := &recordingWriter{createItemRes: &TaskItem{ID: "i-1", Title: "Redigir", Position: 2, CreatedAt: created}}
	app := newAppWithWriter(&recordingReader{}, wr, "tenant-9")

	status, resBody := doJSON(t, app, http.MethodPost, "/v1/tasks/t-1/items", "jwt", `{"title":"Redigir"}`)
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body: %s)", status, resBody)
	}
	if wr.createItemCalls != 1 {
		t.Fatalf("create item calls = %d, want 1", wr.createItemCalls)
	}
	cmd := wr.gotCreateItemCmd
	if cmd.TenantID != "tenant-9" || cmd.TaskID != "t-1" || cmd.Title != "Redigir" {
		t.Errorf("command = %+v, want tenant-9/t-1/Redigir", cmd)
	}
	for _, want := range []string{`"id":"i-1"`, `"title":"Redigir"`, `"position":2`, `"done":false`} {
		if !strings.Contains(resBody, want) {
			t.Errorf("response missing %s\ngot: %s", want, resBody)
		}
	}
}

// An empty title is a 400 at the edge; the use case is never called.
func TestHandler_CreateTaskItem_EmptyTitle_400(t *testing.T) {
	t.Parallel()

	wr := &recordingWriter{}
	app := newAppWithWriter(&recordingReader{}, wr, "tenant-9")

	status, body := doJSON(t, app, http.MethodPost, "/v1/tasks/t-1/items", "jwt", `{"title":""}`)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", status, body)
	}
	if wr.createItemCalls != 0 {
		t.Errorf("create item calls = %d, want 0 (rejected at edge)", wr.createItemCalls)
	}
}

// A foreign/unknown parent task is the use case's typed ErrTaskItemNotFound → 404.
func TestHandler_CreateTaskItem_ParentNotFound_404(t *testing.T) {
	t.Parallel()

	wr := &recordingWriter{createItemErr: ErrTaskItemNotFound}
	app := newAppWithWriter(&recordingReader{}, wr, "tenant-9")

	status, body := doJSON(t, app, http.MethodPost, "/v1/tasks/missing/items", "jwt", `{"title":"x"}`)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body: %s)", status, body)
	}
	if !strings.Contains(body, string(apperr.KindNotFound)) {
		t.Errorf("body missing kind %q\ngot: %s", apperr.KindNotFound, body)
	}
}

// --- PATCH /v1/tasks/:id/items/:itemId --------------------------------------

// The patch handler takes tenant from the PRINCIPAL and both ids from the PATH, forwards only the
// present fields, and returns 200 with the saved item.
func TestHandler_UpdateTaskItem_ForwardsPartialPatch(t *testing.T) {
	t.Parallel()

	done := time.Date(2024, 3, 20, 9, 0, 0, 0, time.UTC)
	wr := &recordingWriter{updateItemRes: &TaskItem{ID: "i-1", Title: "Redigir", Done: true, DoneAt: &done}}
	app := newAppWithWriter(&recordingReader{}, wr, "tenant-9")

	status, resBody := doJSON(t, app, http.MethodPatch, "/v1/tasks/t-1/items/i-1", "jwt", `{"done":true}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", status, resBody)
	}
	if wr.updateItemCalls != 1 {
		t.Fatalf("update item calls = %d, want 1", wr.updateItemCalls)
	}
	cmd := wr.gotUpdateItemCmd
	if cmd.TenantID != "tenant-9" || cmd.TaskID != "t-1" || cmd.ItemID != "i-1" {
		t.Errorf("ids = tenant %q / task %q / item %q, want tenant-9/t-1/i-1", cmd.TenantID, cmd.TaskID, cmd.ItemID)
	}
	if cmd.Done == nil || !*cmd.Done {
		t.Errorf("done = %v, want present true", cmd.Done)
	}
	if cmd.Title != nil {
		t.Errorf("title carried non-nil (%v), want absent", cmd.Title)
	}
	if !strings.Contains(resBody, `"done":true`) {
		t.Errorf("response missing done:true\ngot: %s", resBody)
	}
}

// A present-but-blank title is a 400; the use case is never called.
func TestHandler_UpdateTaskItem_BlankTitle_400(t *testing.T) {
	t.Parallel()

	wr := &recordingWriter{}
	app := newAppWithWriter(&recordingReader{}, wr, "tenant-9")

	status, body := doJSON(t, app, http.MethodPatch, "/v1/tasks/t-1/items/i-1", "jwt", `{"title":""}`)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", status, body)
	}
	if wr.updateItemCalls != 0 {
		t.Errorf("update item calls = %d, want 0 (rejected at edge)", wr.updateItemCalls)
	}
}

// A miss is the use case's typed ErrTaskItemNotFound → 404.
func TestHandler_UpdateTaskItem_NotFound_404(t *testing.T) {
	t.Parallel()

	wr := &recordingWriter{updateItemErr: ErrTaskItemNotFound}
	app := newAppWithWriter(&recordingReader{}, wr, "tenant-9")

	status, body := doJSON(t, app, http.MethodPatch, "/v1/tasks/t-1/items/missing", "jwt", `{"done":true}`)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body: %s)", status, body)
	}
}

// --- DELETE /v1/tasks/:id/items/:itemId -------------------------------------

// The delete handler takes tenant from the PRINCIPAL and both ids from the PATH, and returns 204.
func TestHandler_DeleteTaskItem_ForwardsFromPrincipalAndPath_204(t *testing.T) {
	t.Parallel()

	wr := &recordingWriter{}
	app := newAppWithWriter(&recordingReader{}, wr, "tenant-9")

	status, body := doJSON(t, app, http.MethodDelete, "/v1/tasks/t-1/items/i-1", "jwt", "")
	if status != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (body: %s)", status, body)
	}
	if wr.deleteItemCalls != 1 || wr.gotDeleteItemTenant != "tenant-9" || wr.gotDeleteItemTask != "t-1" || wr.gotDeleteItemID != "i-1" {
		t.Errorf("delete forwarded calls/tenant/task/item = %d/%q/%q/%q, want 1/tenant-9/t-1/i-1",
			wr.deleteItemCalls, wr.gotDeleteItemTenant, wr.gotDeleteItemTask, wr.gotDeleteItemID)
	}
}

// A miss is the use case's typed ErrTaskItemNotFound → 404.
func TestHandler_DeleteTaskItem_NotFound_404(t *testing.T) {
	t.Parallel()

	wr := &recordingWriter{deleteItemErr: ErrTaskItemNotFound}
	app := newAppWithWriter(&recordingReader{}, wr, "tenant-9")

	status, body := doJSON(t, app, http.MethodDelete, "/v1/tasks/t-1/items/missing", "jwt", "")
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body: %s)", status, body)
	}
}

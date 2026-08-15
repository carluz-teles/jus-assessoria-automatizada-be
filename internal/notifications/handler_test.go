package notifications

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

// stubVerifier accepts any bearer token — Auth's job in these tests is only to gate
// on the token's presence, not to test Clerk.
type stubVerifier struct{}

func (stubVerifier) Verify(context.Context, string) (userID, orgID, role string, err error) {
	return "clerk-user", "clerk-org", "", nil
}

// stubResolver returns a principal with the configured user and tenant, standing in
// for the identity slice's resolver.
type stubResolver struct {
	user   string
	tenant string
}

func (r stubResolver) Resolve(context.Context, string, string) (httpx.Principal, error) {
	return httpx.Principal{UserID: r.user, TenantID: r.tenant, Role: "LAWYER"}, nil
}

// fakeReader records the scope the handler passed and returns canned results, so the
// handler tests exercise routing/envelope/status without a database.
type fakeReader struct {
	listResp    []NotificationView
	listHasMore bool
	countResp   int
	markReadErr error
	markAllErr  error

	gotTenant string
	gotUser   string
	gotQuery  ListNotificationsQuery
	gotMarkID string
	markedAll bool
}

func (f *fakeReader) List(_ context.Context, q ListNotificationsQuery) (NotificationsResult, error) {
	f.gotQuery = q
	f.gotTenant, f.gotUser = q.TenantID, q.UserID
	return NotificationsResult{Items: f.listResp, HasMore: f.listHasMore}, nil
}

func (f *fakeReader) UnreadCount(_ context.Context, tenantID, userID string) (int, error) {
	f.gotTenant, f.gotUser = tenantID, userID
	return f.countResp, nil
}

func (f *fakeReader) MarkRead(_ context.Context, tenantID, userID, notificationID string) error {
	f.gotTenant, f.gotUser, f.gotMarkID = tenantID, userID, notificationID
	return f.markReadErr
}

func (f *fakeReader) MarkAllRead(_ context.Context, tenantID, userID string) error {
	f.gotTenant, f.gotUser, f.markedAll = tenantID, userID, true
	return f.markAllErr
}

// fakePrefs records the scope the handler passed and returns canned results, so the
// preference handler tests exercise routing/envelope/status without a database.
type fakePrefs struct {
	getResp []NotificationPreference
	getErr  error
	setResp *NotificationPreference
	setErr  error

	gotTenant   string
	gotUser     string
	gotType     string
	gotChannels []string
}

func (f *fakePrefs) GetPreferences(_ context.Context, tenantID, appUserID string) ([]NotificationPreference, error) {
	f.gotTenant, f.gotUser = tenantID, appUserID
	return f.getResp, f.getErr
}

func (f *fakePrefs) SetPreference(_ context.Context, tenantID, appUserID, notifType string, channels []string) (*NotificationPreference, error) {
	f.gotTenant, f.gotUser, f.gotType, f.gotChannels = tenantID, appUserID, notifType, channels
	return f.setResp, f.setErr
}

// newApp builds an app whose /v1 group mirrors production: Auth resolves a principal
// with the given user/tenant, then the notifications routes mount under it. Tests
// that only exercise the inbox routes pass a zero-value fakePrefs implicitly — use
// newAppWithPrefs for the preference routes.
func newApp(r reader, user, tenant string) *fiber.App {
	return newAppWithPrefs(r, &fakePrefs{}, user, tenant)
}

// newAppWithPrefs is newApp with an explicit prefsUC double, for the preference
// route tests.
func newAppWithPrefs(r reader, prefs prefsUC, user, tenant string) *fiber.App {
	app := fiber.New(fiber.Config{
		ErrorHandler: func(c *fiber.Ctx, err error) error { return httpx.WriteError(c, err) },
	})
	v1 := app.Group("/v1", middleware.Auth(stubVerifier{}, stubResolver{user: user, tenant: tenant}))
	NewHandler(r, closedSubscriber{}, prefs).Register(v1)
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

// doBody is do, plus a JSON request body — for the PUT preferences route.
func doBody(t *testing.T, app *fiber.App, method, path, body, bearer string) (int, string) {
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

// --- tests -------------------------------------------------------------------

// AC6: no bearer token → 401 at the auth boundary; the handler never runs.
func TestHandler_NoToken_401(t *testing.T) {
	t.Parallel()

	app := newApp(&fakeReader{}, "u-1", "tenant-1")
	for _, path := range []string{"/v1/notifications", "/v1/notifications/unread-count"} {
		if status, _ := do(t, app, http.MethodGet, path, ""); status != http.StatusUnauthorized {
			t.Fatalf("GET %s without token: status = %d, want 401", path, status)
		}
	}
}

// AC1: GET /v1/notifications returns the {data, page:{next_cursor,limit}} envelope,
// scoped to the principal's tenant AND user; a full page carries a next_cursor keyed
// off the last row's created_at.
func TestHandler_List_EnvelopeAndScope(t *testing.T) {
	t.Parallel()

	created := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	fr := &fakeReader{
		listResp:    []NotificationView{{ID: "n-1", Type: TypeNewAndamento, CreatedAt: created}},
		listHasMore: true,
	}
	app := newApp(fr, "u-7", "tenant-42")

	status, body := do(t, app, http.MethodGet, "/v1/notifications?limit=1", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, body)
	}
	if fr.gotTenant != "tenant-42" || fr.gotUser != "u-7" {
		t.Fatalf("scope = (%q,%q), want (tenant-42,u-7) from principal", fr.gotTenant, fr.gotUser)
	}

	var env httpx.Page[NotificationView]
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decode envelope: %v; body=%s", err, body)
	}
	if len(env.Data) != 1 || env.Data[0].ID != "n-1" {
		t.Fatalf("data = %+v, want the one aviso", env.Data)
	}
	if env.Page.Limit != 1 {
		t.Errorf("page.limit = %d, want 1", env.Page.Limit)
	}
	if env.Page.NextCursor == nil || *env.Page.NextCursor == "" {
		t.Errorf("page.next_cursor = %v, want a token (hasMore)", env.Page.NextCursor)
	}
}

// AC1: ?unread=true flows through to the query as the per-user unread filter.
func TestHandler_List_UnreadFilterFlows(t *testing.T) {
	t.Parallel()

	fr := &fakeReader{}
	app := newApp(fr, "u-1", "tenant-1")

	if status, body := do(t, app, http.MethodGet, "/v1/notifications?unread=true", "jwt"); status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, body)
	}
	if !fr.gotQuery.UnreadOnly {
		t.Errorf("query.UnreadOnly = false, want true (?unread=true)")
	}
}

// AC1: ?type=<closed value> flows through to the query; an out-of-set type is a
// client error → 400 (the handler is the app-level CHECK on the type column).
func TestHandler_List_TypeFilterFlowsAndValidates(t *testing.T) {
	t.Parallel()

	fr := &fakeReader{}
	app := newApp(fr, "u-1", "tenant-1")

	if status, body := do(t, app, http.MethodGet, "/v1/notifications?type=deadline_missed", "jwt"); status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, body)
	}
	if fr.gotQuery.Type != TypeDeadlineMissedAviso {
		t.Errorf("query.Type = %q, want %q", fr.gotQuery.Type, TypeDeadlineMissedAviso)
	}

	status, body := do(t, app, http.MethodGet, "/v1/notifications?type=alarm", "jwt")
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for unknown type; body=%s", status, body)
	}
	if !strings.Contains(body, string(apperr.KindInvalid)) {
		t.Errorf("body missing kind %q\ngot: %s", apperr.KindInvalid, body)
	}
}

// AC1: a param outside the inbox's allowlist is rejected with 400, never silently
// dropped (docs/erd-backend.md §4e.3).
func TestHandler_List_UnknownParam_400(t *testing.T) {
	t.Parallel()

	fr := &fakeReader{}
	app := newApp(fr, "u-1", "tenant-1")

	if status, body := do(t, app, http.MethodGet, "/v1/notifications?channel=email", "jwt"); status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", status, body)
	}
}

// AC1: the filters block is always a JSON object, never null — a zero result must
// serialize as {} so the FE renders an empty chip row, not a blank block. (The closed
// type options themselves are assembled by the use case — asserted in read_test.go.)
func TestHandler_List_EnvelopeFiltersAlwaysObject(t *testing.T) {
	t.Parallel()

	fr := &fakeReader{}
	app := newApp(fr, "u-1", "tenant-1")

	status, body := do(t, app, http.MethodGet, "/v1/notifications", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, body)
	}
	if !strings.Contains(body, `"filters":{}`) {
		t.Errorf("envelope filters not an empty object\ngot: %s", body)
	}
}

// AC2: GET /v1/notifications/unread-count returns {"count": N}.
func TestHandler_UnreadCount(t *testing.T) {
	t.Parallel()

	fr := &fakeReader{countResp: 5}
	app := newApp(fr, "u-1", "tenant-1")

	status, body := do(t, app, http.MethodGet, "/v1/notifications/unread-count", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, body)
	}
	var got unreadCountResponse
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode: %v; body=%s", err, body)
	}
	if got.Count != 5 {
		t.Errorf("count = %d, want 5", got.Count)
	}
}

// AC3: POST /v1/notifications/:id/read → 204, id and principal scope forwarded.
func TestHandler_MarkRead_204(t *testing.T) {
	t.Parallel()

	fr := &fakeReader{}
	app := newApp(fr, "u-3", "tenant-3")

	status, body := do(t, app, http.MethodPost, "/v1/notifications/aviso-9/read", "jwt")
	if status != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", status, body)
	}
	if fr.gotMarkID != "aviso-9" || fr.gotTenant != "tenant-3" || fr.gotUser != "u-3" {
		t.Fatalf("mark = (id=%q,tenant=%q,user=%q), want (aviso-9,tenant-3,u-3)", fr.gotMarkID, fr.gotTenant, fr.gotUser)
	}
}

// AC3: an aviso the use case reports as not-found (another tenant's) → 404 with the
// ENTITY_NOT_FOUND envelope kind.
func TestHandler_MarkRead_NotFound_404(t *testing.T) {
	t.Parallel()

	fr := &fakeReader{markReadErr: ErrNotificationNotFound}
	app := newApp(fr, "u-1", "tenant-1")

	status, body := do(t, app, http.MethodPost, "/v1/notifications/other-tenants-aviso/read", "jwt")
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", status, body)
	}
	var env httpx.ErrorBody
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decode error body: %v; body=%s", err, body)
	}
	if env.Kind != "ENTITY_NOT_FOUND" {
		t.Errorf("kind = %q, want ENTITY_NOT_FOUND", env.Kind)
	}
}

// AC4: POST /v1/notifications/read-all → 204, scoped to the principal.
func TestHandler_MarkAllRead_204(t *testing.T) {
	t.Parallel()

	fr := &fakeReader{}
	app := newApp(fr, "u-2", "tenant-2")

	status, body := do(t, app, http.MethodPost, "/v1/notifications/read-all", "jwt")
	if status != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", status, body)
	}
	if !fr.markedAll || fr.gotTenant != "tenant-2" || fr.gotUser != "u-2" {
		t.Fatalf("markAll = %v scope (%q,%q), want true (tenant-2,u-2)", fr.markedAll, fr.gotTenant, fr.gotUser)
	}
}

// --- GET/PUT /v1/notifications/preferences ------------------------------------

// AC2: GET returns the caller's saved overrides, scoped to the principal
// (tenant_id/app_user_id never come from the request).
func TestHandler_GetPreferences_200(t *testing.T) {
	t.Parallel()

	fp := &fakePrefs{getResp: []NotificationPreference{{Type: "member_joined", Channels: []string{"IN_APP"}}}}
	app := newAppWithPrefs(&fakeReader{}, fp, "u-1", "tenant-1")

	status, body := do(t, app, http.MethodGet, "/v1/notifications/preferences", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, body)
	}
	if fp.gotTenant != "tenant-1" || fp.gotUser != "u-1" {
		t.Fatalf("scope = (%q, %q), want (tenant-1, u-1)", fp.gotTenant, fp.gotUser)
	}
	if !strings.Contains(body, `"type":"member_joined"`) || !strings.Contains(body, `"channels":["IN_APP"]`) {
		t.Fatalf("body missing saved override: %s", body)
	}
}

// AC2: no overrides saved → 200 with data:[], never null.
func TestHandler_GetPreferences_Empty_200(t *testing.T) {
	t.Parallel()

	fp := &fakePrefs{}
	app := newAppWithPrefs(&fakeReader{}, fp, "u-1", "tenant-1")

	status, body := do(t, app, http.MethodGet, "/v1/notifications/preferences", "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, body)
	}
	if !strings.Contains(body, `"data":[]`) {
		t.Errorf("empty preferences should serialize as data:[], got: %s", body)
	}
}

// No bearer token → 401; the use case never runs.
func TestHandler_GetPreferences_NoToken_401(t *testing.T) {
	t.Parallel()

	fp := &fakePrefs{}
	app := newAppWithPrefs(&fakeReader{}, fp, "u-1", "tenant-1")

	status, _ := do(t, app, http.MethodGet, "/v1/notifications/preferences", "")
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
	if fp.gotTenant != "" {
		t.Fatal("use case ran despite a missing token")
	}
}

// AC2: PUT saves the caller's own preference — tenant_id/app_user_id come from the
// principal, never the body (the request carries no such fields to spoof).
func TestHandler_SetPreference_200(t *testing.T) {
	t.Parallel()

	fp := &fakePrefs{setResp: &NotificationPreference{Type: "member_joined", Channels: []string{"IN_APP"}}}
	app := newAppWithPrefs(&fakeReader{}, fp, "u-1", "tenant-1")

	body := `{"type":"member_joined","channels":["IN_APP"]}`
	status, resp := doBody(t, app, http.MethodPut, "/v1/notifications/preferences", body, "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, resp)
	}
	if fp.gotTenant != "tenant-1" || fp.gotUser != "u-1" || fp.gotType != "member_joined" {
		t.Fatalf("scope = (%q, %q, %q), want (tenant-1, u-1, member_joined)", fp.gotTenant, fp.gotUser, fp.gotType)
	}
	if len(fp.gotChannels) != 1 || fp.gotChannels[0] != "IN_APP" {
		t.Fatalf("channels = %v, want [IN_APP]", fp.gotChannels)
	}
}

// AC2: an empty channels array is a VALID explicit full opt-out, not a validation
// error.
func TestHandler_SetPreference_EmptyChannels_200(t *testing.T) {
	t.Parallel()

	fp := &fakePrefs{setResp: &NotificationPreference{Type: "member_joined", Channels: []string{}}}
	app := newAppWithPrefs(&fakeReader{}, fp, "u-1", "tenant-1")

	body := `{"type":"member_joined","channels":[]}`
	status, resp := doBody(t, app, http.MethodPut, "/v1/notifications/preferences", body, "jwt")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, resp)
	}
	if fp.gotChannels == nil || len(fp.gotChannels) != 0 {
		t.Fatalf("channels = %v, want an empty (non-nil) slice", fp.gotChannels)
	}
}

// AC2: an unknown channel value → 400; the use case never runs.
func TestHandler_SetPreference_InvalidChannel_400(t *testing.T) {
	t.Parallel()

	fp := &fakePrefs{}
	app := newAppWithPrefs(&fakeReader{}, fp, "u-1", "tenant-1")

	body := `{"type":"member_joined","channels":["FAX"]}`
	status, resp := doBody(t, app, http.MethodPut, "/v1/notifications/preferences", body, "jwt")
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", status, resp)
	}
	if fp.gotType != "" {
		t.Fatal("use case ran on an invalid channel")
	}
}

// AC2: a missing type → 400; the use case never runs.
func TestHandler_SetPreference_MissingType_400(t *testing.T) {
	t.Parallel()

	fp := &fakePrefs{}
	app := newAppWithPrefs(&fakeReader{}, fp, "u-1", "tenant-1")

	body := `{"channels":["EMAIL"]}`
	status, resp := doBody(t, app, http.MethodPut, "/v1/notifications/preferences", body, "jwt")
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", status, resp)
	}
	if fp.gotType != "" {
		t.Fatal("use case ran despite a missing type")
	}
}

// No bearer token → 401; the use case never runs.
func TestHandler_SetPreference_NoToken_401(t *testing.T) {
	t.Parallel()

	fp := &fakePrefs{}
	app := newAppWithPrefs(&fakeReader{}, fp, "u-1", "tenant-1")

	body := `{"type":"member_joined","channels":["EMAIL"]}`
	status, _ := doBody(t, app, http.MethodPut, "/v1/notifications/preferences", body, "")
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
	if fp.gotType != "" {
		t.Fatal("use case ran despite a missing token")
	}
}

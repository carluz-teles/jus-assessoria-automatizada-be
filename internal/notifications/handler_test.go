package notifications

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"

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

func (f *fakeReader) List(_ context.Context, q ListNotificationsQuery) ([]NotificationView, bool, error) {
	f.gotQuery = q
	f.gotTenant, f.gotUser = q.TenantID, q.UserID
	return f.listResp, f.listHasMore, nil
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

// newApp builds an app whose /v1 group mirrors production: Auth resolves a principal
// with the given user/tenant, then the notifications routes mount under it.
func newApp(r reader, user, tenant string) *fiber.App {
	app := fiber.New(fiber.Config{
		ErrorHandler: func(c *fiber.Ctx, err error) error { return httpx.WriteError(c, err) },
	})
	v1 := app.Group("/v1", middleware.Auth(stubVerifier{}, stubResolver{user: user, tenant: tenant}))
	NewHandler(r).Register(v1)
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

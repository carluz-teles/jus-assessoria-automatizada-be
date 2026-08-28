package draft

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

// panicIterator implements the iterator port and fails the test if Iterate is
// ever invoked — used by tests that must never reach the use case (e.g. a
// malformed body must fail at BodyParser, before Iterate is called).
type panicIterator struct{ t *testing.T }

func (p panicIterator) Iterate(context.Context, IterateCommand) (*IterateResult, error) {
	p.t.Fatal("Iterate must not be called")
	return nil, nil
}

// newAppWithIterator wires a Handler with the given iterator under the Auth
// boundary, mirroring production's /v1 group.
func newAppWithIterator(iter iterator, tenant string) *fiber.App {
	app := fiber.New(fiber.Config{
		ErrorHandler: func(c *fiber.Ctx, err error) error { return httpx.WriteError(c, err) },
	})
	v1 := app.Group("/v1", middleware.Auth(stubVerifier{}, stubResolver{tenant: tenant}))
	NewHandler(nil).WithIterator(iter).RegisterV1(v1)
	return app
}

// fakeLister is a configurable stub satisfying the lister port, used to pin the
// page envelope shape (total_count/total) the list endpoints return. gotListAllQuery
// records the last ListAllQuery received, so tests can assert what the handler
// resolved (e.g. the ?assignee filter) without a real database.
type fakeLister struct {
	result DraftListResult
	err    error

	gotListAllQuery *ListAllQuery
}

func (f *fakeLister) ListByProcess(context.Context, ListByProcessQuery) (DraftListResult, error) {
	return f.result, f.err
}

func (f *fakeLister) ListAll(_ context.Context, q ListAllQuery) (DraftListResult, error) {
	f.gotListAllQuery = &q
	return f.result, f.err
}

// newAppWithLister wires a Handler with the given lister under the Auth boundary,
// mirroring production's /v1 group.
func newAppWithLister(l lister, tenant string) *fiber.App {
	app := fiber.New(fiber.Config{
		ErrorHandler: func(c *fiber.Ctx, err error) error { return httpx.WriteError(c, err) },
	})
	v1 := app.Group("/v1", middleware.Auth(stubVerifier{}, stubResolver{tenant: tenant}))
	NewHandler(nil).WithLister(l).RegisterV1(v1)
	return app
}

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

// TestHandler_IteratePeca_MalformedBody_400 pins the fix for a QA-found bug:
// a malformed body (scope as a string instead of an object) must fail as a
// client error (400 DOMAIN_ERROR_INVALID), not escape as a 500 INFRA_ERROR.
// The use case must never be reached.
func TestHandler_IteratePeca_MalformedBody_400(t *testing.T) {
	t.Parallel()

	app := newAppWithIterator(panicIterator{t: t}, "tenant-1")

	status, body := doJSON(t, app, http.MethodPost, "/v1/pecas/draft-1/iterate", "jwt", `{"scope":"whole"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", status, body)
	}

	var got httpx.ErrorBody
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("unmarshal error body: %v (body: %s)", err, body)
	}
	if got.Kind != string(apperr.KindInvalid) {
		t.Errorf("kind = %q, want %q", got.Kind, apperr.KindInvalid)
	}
}

// pageEnvelope decodes only the {data, page} shape needed to assert the total —
// it deliberately ignores the per-item fields (covered by mapper tests).
type pageEnvelope struct {
	Data []map[string]any `json:"data"`
	Page httpx.PageMeta   `json:"page"`
}

// TestHandler_ListPecas_Total pins the fix for a QA-found bug: GET /v1/pecas always
// returned page.total_count/page.total = 0 even when data had real items, because
// newDraftListPage never read a total off the result. Covers both list endpoints
// that share DraftListResult/newDraftListPage: the tenant library (/v1/pecas) and
// the per-process tab (/v1/processos/:id/pecas).
func TestHandler_ListPecas_Total(t *testing.T) {
	t.Parallel()

	now := time.Now()
	items := []DraftListItem{
		{ID: newDraftID(), PieceType: PieceTypeDefense, Status: StatusDraft, CreatedAt: now},
		{ID: newDraftID(), PieceType: PieceTypeAppeal, Status: StatusSigned, CreatedAt: now.Add(-time.Hour)},
	}

	tests := []struct {
		name string
		path string
	}{
		{name: "tenant library", path: "/v1/pecas?limit=5"},
		{name: "per-process tab", path: "/v1/processos/case-1/pecas?limit=5"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			app := newAppWithLister(&fakeLister{result: DraftListResult{Items: items, Total: 7}}, "tenant-1")

			status, body := doJSON(t, app, http.MethodGet, tt.path, "jwt", "")
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body: %s)", status, body)
			}

			var got pageEnvelope
			if err := json.Unmarshal([]byte(body), &got); err != nil {
				t.Fatalf("unmarshal body: %v (body: %s)", err, body)
			}
			if len(got.Data) != len(items) {
				t.Errorf("len(data) = %d, want %d", len(got.Data), len(items))
			}
			if got.Page.TotalCount != 7 {
				t.Errorf("page.total_count = %d, want 7", got.Page.TotalCount)
			}
			if got.Page.Total != 7 {
				t.Errorf("page.total = %d, want 7", got.Page.Total)
			}
		})
	}
}

// TestHandler_ListPecas_AssigneeFilter covers the ?assignee filter (the FE "Minhas"
// chip): "" is no filter, "me" resolves to the authenticated principal's id
// (stubResolver returns UserID "u-1"), an explicit uuid passes through verbatim, and
// a malformed value is a 400 before the use case is ever reached.
func TestHandler_ListPecas_AssigneeFilter(t *testing.T) {
	t.Parallel()

	otherUserID := newDraftID() // any well-formed uuid stands in for another user's id

	tests := []struct {
		name         string
		path         string
		wantStatus   int
		wantAssignee string // only checked when wantStatus == 200
	}{
		{name: "no filter", path: "/v1/pecas", wantStatus: http.StatusOK, wantAssignee: ""},
		{name: "me resolves to principal id", path: "/v1/pecas?assignee=me", wantStatus: http.StatusOK, wantAssignee: "u-1"},
		{name: "explicit uuid passes through", path: "/v1/pecas?assignee=" + otherUserID, wantStatus: http.StatusOK, wantAssignee: otherUserID},
		{name: "malformed value is 400", path: "/v1/pecas?assignee=not-a-uuid", wantStatus: http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			lister := &fakeLister{result: DraftListResult{Items: []DraftListItem{}, Total: 0}}
			app := newAppWithLister(lister, "tenant-1")

			status, body := doJSON(t, app, http.MethodGet, tt.path, "jwt", "")
			if status != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body: %s)", status, tt.wantStatus, body)
			}
			if tt.wantStatus != http.StatusOK {
				return
			}
			if lister.gotListAllQuery == nil {
				t.Fatal("ListAll was not called")
			}
			if lister.gotListAllQuery.Assignee != tt.wantAssignee {
				t.Errorf("Assignee = %q, want %q", lister.gotListAllQuery.Assignee, tt.wantAssignee)
			}
		})
	}
}

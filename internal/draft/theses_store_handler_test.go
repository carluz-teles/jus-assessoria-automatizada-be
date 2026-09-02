package draft

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/jusassessoria/platform/lib/httpx"
	"github.com/jusassessoria/platform/lib/httpx/middleware"
)

// fakeDraftThesesStore satisfies the draftThesesStore handler port.
type fakeDraftThesesStore struct {
	list       []SuggestedThesis
	generated  []SuggestedThesis
	updated    *SuggestedThesis
	genErr     error
	updateErr  error
	gotState   string
	gotThesisI string
}

func (f *fakeDraftThesesStore) GenerateDraftTheses(context.Context, string, string) ([]SuggestedThesis, error) {
	return f.generated, f.genErr
}
func (f *fakeDraftThesesStore) ListDraftTheses(context.Context, string, string) ([]SuggestedThesis, error) {
	return f.list, nil
}
func (f *fakeDraftThesesStore) UpdateThesisState(_ context.Context, _, thesisID, state string) (*SuggestedThesis, error) {
	f.gotThesisI, f.gotState = thesisID, state
	return f.updated, f.updateErr
}
func (f *fakeDraftThesesStore) GenerateIntimationTheses(context.Context, string, string) ([]SuggestedThesis, error) {
	return f.generated, f.genErr
}
func (f *fakeDraftThesesStore) ListIntimationTheses(context.Context, string, string) ([]SuggestedThesis, error) {
	return f.list, nil
}

func newAppWithThesesStore(s draftThesesStore) *fiber.App {
	app := fiber.New(fiber.Config{
		ErrorHandler: func(c *fiber.Ctx, err error) error { return httpx.WriteError(c, err) },
	})
	v1 := app.Group("/v1", middleware.Auth(stubVerifier{}, stubResolver{tenant: "t-1"}))
	NewHandler(nil).WithThesesStore(s).RegisterV1(v1)
	return app
}

func doReq(t *testing.T, app *fiber.App, method, path, body string) (*http.Response, map[string]any) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	req.Header.Set("Authorization", "Bearer x")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	var out map[string]any
	b, _ := io.ReadAll(resp.Body)
	if len(b) > 0 {
		_ = json.Unmarshal(b, &out)
	}
	return resp, out
}

func TestHandler_listPecaTheses_shape(t *testing.T) {
	store := &fakeDraftThesesStore{list: []SuggestedThesis{
		{ID: "id-1", State: ThesisStatePendingAdd, Position: 0, Label: "L", Confidence: ThesisConfidenceAlta, Grounded: true, Evidence: []string{"e"}},
	}}
	app := newAppWithThesesStore(store)
	resp, out := doReq(t, app, http.MethodGet, "/v1/pecas/draft-1/theses", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	data, ok := out["data"].([]any)
	if !ok || len(data) != 1 {
		t.Fatalf("expected data array of 1, got %#v", out["data"])
	}
	item := data[0].(map[string]any)
	for _, k := range []string{"id", "state", "position", "label", "confidence", "evidence", "grounded"} {
		if _, present := item[k]; !present {
			t.Errorf("missing field %q in %#v", k, item)
		}
	}
	if item["id"] != "id-1" || item["state"] != ThesisStatePendingAdd {
		t.Errorf("wrong id/state: %#v", item)
	}
}

func TestHandler_listPecaTheses_empty(t *testing.T) {
	app := newAppWithThesesStore(&fakeDraftThesesStore{list: nil})
	resp, out := doReq(t, app, http.MethodGet, "/v1/pecas/draft-1/theses", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if data, ok := out["data"].([]any); !ok || len(data) != 0 {
		t.Fatalf("expected empty array, got %#v", out["data"])
	}
}

func TestHandler_thesesPeca_generatesAndReturnsList(t *testing.T) {
	store := &fakeDraftThesesStore{generated: []SuggestedThesis{
		{ID: "g-1", State: ThesisStateOff, Position: 0, Label: "gen"},
	}}
	app := newAppWithThesesStore(store)
	resp, out := doReq(t, app, http.MethodPost, "/v1/pecas/draft-1/theses", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	data, ok := out["data"].([]any)
	if !ok || len(data) != 1 {
		t.Fatalf("expected 1 generated, got %#v", out["data"])
	}
	if data[0].(map[string]any)["id"] != "g-1" {
		t.Errorf("wrong id: %#v", data[0])
	}
}

func TestHandler_listIntimationTheses_shape(t *testing.T) {
	store := &fakeDraftThesesStore{list: []SuggestedThesis{
		{ID: "id-1", State: ThesisStatePendingAdd, Position: 0, Label: "L", Confidence: ThesisConfidenceAlta, Grounded: true, Evidence: []string{"e"}},
	}}
	app := newAppWithThesesStore(store)
	resp, out := doReq(t, app, http.MethodGet, "/v1/intimacoes/int-1/theses", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	data, ok := out["data"].([]any)
	if !ok || len(data) != 1 {
		t.Fatalf("expected data array of 1 (same shape as draft), got %#v", out["data"])
	}
	if data[0].(map[string]any)["id"] != "id-1" {
		t.Errorf("wrong id: %#v", data[0])
	}
}

func TestHandler_thesesFromIntimation_generatesPersisted(t *testing.T) {
	store := &fakeDraftThesesStore{generated: []SuggestedThesis{
		{ID: "g-1", State: ThesisStatePendingAdd, Position: 0, Label: "gen"},
	}}
	app := newAppWithThesesStore(store)
	resp, out := doReq(t, app, http.MethodPost, "/v1/intimacoes/int-1/theses", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	data, ok := out["data"].([]any)
	if !ok || len(data) != 1 {
		t.Fatalf("expected 1 generated (persisted shape with id/state), got %#v", out["data"])
	}
	item := data[0].(map[string]any)
	if item["id"] != "g-1" || item["state"] != ThesisStatePendingAdd {
		t.Errorf("wrong id/state: %#v", item)
	}
}

func TestHandler_patchThesisState_ok(t *testing.T) {
	store := &fakeDraftThesesStore{updated: &SuggestedThesis{ID: "id-1", State: ThesisStateIncluded, Label: "L"}}
	app := newAppWithThesesStore(store)
	resp, out := doReq(t, app, http.MethodPatch, "/v1/pecas/draft-1/theses/id-1", `{"state":"included"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if store.gotThesisI != "id-1" || store.gotState != ThesisStateIncluded {
		t.Errorf("handler passed thesisID=%q state=%q", store.gotThesisI, store.gotState)
	}
	item := out["data"].(map[string]any)
	if item["state"] != ThesisStateIncluded {
		t.Errorf("wrong state in response: %#v", item)
	}
}

func TestHandler_patchThesisState_notFound(t *testing.T) {
	store := &fakeDraftThesesStore{updateErr: ErrSuggestedThesisNotFound}
	app := newAppWithThesesStore(store)
	resp, _ := doReq(t, app, http.MethodPatch, "/v1/pecas/draft-1/theses/ghost", `{"state":"off"}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestHandler_theses_nilStore_returnsIANotConfigured(t *testing.T) {
	app := newAppWithThesesStore(nil)
	// A nil store means the port is not wired — WithThesesStore(nil) leaves the
	// handler field nil, so all three routes must return ErrIANotConfigured.
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/v1/pecas/draft-1/theses", ""},
		{http.MethodPost, "/v1/pecas/draft-1/theses", ""},
		{http.MethodPatch, "/v1/pecas/draft-1/theses/id-1", `{"state":"off"}`},
	} {
		resp, _ := doReq(t, app, tc.method, tc.path, tc.body)
		if resp.StatusCode == http.StatusOK {
			t.Errorf("%s %s: expected non-200 (IA não configurada), got 200", tc.method, tc.path)
		}
	}
}

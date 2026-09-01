package actionitem

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/jusassessoria/platform/lib/httpx"
)

// newTestApp wires the real Handler behind a fiber app that stamps the given tenant onto
// every request's Principal — standing in for the auth middleware, which is out of scope
// for this slice's handler tests.
func newTestApp(h *Handler, tenantID string) *fiber.App {
	app := fiber.New()
	app.Use(func(c *fiber.Ctx) error {
		httpx.SetPrincipal(c, httpx.Principal{TenantID: tenantID})
		return c.Next()
	})
	h.RegisterV1(app.Group(""))
	return app
}

// TestConfirmarResponseIsSnakeCase reproduces the bug QA found: confirmar/descartar used
// to serialize *ActionItem (entity.go, no json tags) directly, so the HTTP response came
// out PascalCase ("ID", "TenantID", "TipoOrigem", ...) instead of the snake_case every
// other slice's DTO uses (see internal/deadline/handler.go, internal/pieceprofile/
// handler.go). newActionItemResponse (handler.go) is the fix; this test asserts the actual
// wire shape, not just that the DTO type has tags.
func TestConfirmarResponseIsSnakeCase(t *testing.T) {
	confianca := 0.7
	repo := newMockRepo()
	repo.seed(&ActionItem{
		ID: "a1", TenantID: "t1", IntimationID: "i1",
		Tipo: TipoManifestar, TipoOrigem: TipoOrigemIA, TipoStatus: TipoStatusAConfirmar,
		Status: StatusSuggested, Confianca: &confianca,
	})
	uc := NewUseCase(repo, &recordingOutbox{}, &fakeDedup{}, &fakeUOW{})
	app := newTestApp(NewHandler(uc), "t1")

	req := httptest.NewRequest("POST", "/action-items/a1/confirmar", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test() error = %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}

	// The bug: PascalCase field names leaking straight from the Go struct.
	for _, leaked := range []string{`"ID"`, `"TenantID"`, `"TipoOrigem"`, `"TipoStatus"`} {
		if strings.Contains(string(body), leaked) {
			t.Errorf("response leaked PascalCase field %s: %s", leaked, body)
		}
	}

	var parsed struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("json.Unmarshal: %v, body = %s", err, body)
	}

	for _, wantKey := range []string{
		"id", "tenant_id", "intimation_id", "tipo", "gera_peca",
		"tipo_origem", "tipo_status", "confianca", "status",
		"created_at", "updated_at",
	} {
		if _, ok := parsed.Data[wantKey]; !ok {
			t.Errorf("response missing snake_case key %q, got keys %v", wantKey, keysOf(parsed.Data))
		}
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

package httpx_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	validation "github.com/go-ozzo/ozzo-validation/v4"
	"github.com/gofiber/fiber/v2"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/httpx"
)

// decodedBody is the wire shape of ErrorBody as the client sees it. Details is
// decoded as a string map because ozzo's validation.Errors marshals to
// {field: message}.
type decodedBody struct {
	Kind    string            `json:"kind"`
	Message string            `json:"message"`
	Details map[string]string `json:"details"`
}

// runHandler wires h onto a fresh Fiber app and drives one GET request through
// it, returning the status and decoded body plus the raw payload for leak checks.
func runHandler(t *testing.T, h fiber.Handler) (int, decodedBody, string) {
	t.Helper()

	app := fiber.New()
	app.Get("/", h)

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	var body decodedBody
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode body %q: %v", raw, err)
	}

	return resp.StatusCode, body, string(raw)
}

func TestWriteError_KindMapsToStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantKind   string
	}{
		{
			name:       "invalid",
			err:        apperr.NewInvalid("tipo de peça inválido"),
			wantStatus: http.StatusBadRequest,
			wantKind:   string(apperr.KindInvalid),
		},
		{
			name:       "unauthorized",
			err:        apperr.NewUnauthorized("token inválido"),
			wantStatus: http.StatusUnauthorized,
			wantKind:   string(apperr.KindUnauthorized),
		},
		{
			name:       "forbidden",
			err:        apperr.NewForbidden("acesso negado"),
			wantStatus: http.StatusForbidden,
			wantKind:   string(apperr.KindForbidden),
		},
		{
			name:       "not found",
			err:        apperr.NewNotFound("minuta não encontrada"),
			wantStatus: http.StatusNotFound,
			wantKind:   string(apperr.KindNotFound),
		},
		{
			name:       "conflict",
			err:        apperr.NewConflict("estado já publicado"),
			wantStatus: http.StatusConflict,
			wantKind:   string(apperr.KindConflict),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			status, body, _ := runHandler(t, func(c *fiber.Ctx) error {
				return httpx.WriteError(c, tt.err)
			})

			if status != tt.wantStatus {
				t.Errorf("status = %d, want %d", status, tt.wantStatus)
			}
			if body.Kind != tt.wantKind {
				t.Errorf("kind = %q, want %q", body.Kind, tt.wantKind)
			}
		})
	}
}

func TestWriteError_NotFoundBody(t *testing.T) {
	t.Parallel()

	status, body, _ := runHandler(t, func(c *fiber.Ctx) error {
		return httpx.WriteError(c, apperr.NewNotFound("minuta não encontrada"))
	})

	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", status)
	}
	if body.Kind != "ENTITY_NOT_FOUND" {
		t.Errorf("kind = %q, want ENTITY_NOT_FOUND", body.Kind)
	}
	// Sub-500: the safe AppError.Message reaches the client verbatim.
	if body.Message != "minuta não encontrada" {
		t.Errorf("message = %q, want the AppError message", body.Message)
	}
}

func TestWriteError_PlainErrorIsGeneric500(t *testing.T) {
	t.Parallel()

	const secret = "connection to 10.0.0.1 refused: password=hunter2"

	status, body, raw := runHandler(t, func(c *fiber.Ctx) error {
		return httpx.WriteError(c, errors.New(secret))
	})

	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", status)
	}
	if body.Kind != string(apperr.KindInfra) {
		t.Errorf("kind = %q, want %q", body.Kind, apperr.KindInfra)
	}
	if body.Message != "internal error" {
		t.Errorf("message = %q, want generic %q", body.Message, "internal error")
	}
	// The cause MUST NOT leak into the client payload.
	if strings.Contains(raw, "hunter2") || strings.Contains(raw, "10.0.0.1") {
		t.Errorf("cause leaked into body: %s", raw)
	}
}

// sampleReq mirrors a slice Request with an ozzo Validate method.
type sampleReq struct {
	Title string `json:"title"`
}

func (r sampleReq) validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.Title, validation.Required),
	)
}

func TestWriteValidationError_FieldMap(t *testing.T) {
	t.Parallel()

	verr := sampleReq{}.validate()
	if verr == nil {
		t.Fatal("expected validation to fail on empty title")
	}
	if _, ok := verr.(validation.Errors); !ok {
		t.Fatalf("expected validation.Errors, got %T", verr)
	}

	status, body, _ := runHandler(t, func(c *fiber.Ctx) error {
		return httpx.WriteValidationError(c, verr)
	})

	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
	if body.Kind != string(apperr.KindInvalid) {
		t.Errorf("kind = %q, want %q", body.Kind, apperr.KindInvalid)
	}
	if body.Message != "validation failed" {
		t.Errorf("message = %q, want %q", body.Message, "validation failed")
	}
	if _, ok := body.Details["title"]; !ok {
		t.Errorf("details = %v, want a message under the \"title\" field", body.Details)
	}
}

func TestWriteValidationError_FallsBackToWriteError(t *testing.T) {
	t.Parallel()

	// A non-validation error must not be mislabelled as a validation failure.
	status, body, _ := runHandler(t, func(c *fiber.Ctx) error {
		return httpx.WriteValidationError(c, apperr.NewConflict("estado já publicado"))
	})

	if status != http.StatusConflict {
		t.Fatalf("status = %d, want 409", status)
	}
	if body.Kind != string(apperr.KindConflict) {
		t.Errorf("kind = %q, want %q", body.Kind, apperr.KindConflict)
	}
}

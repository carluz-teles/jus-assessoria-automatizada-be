package httpx_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

// capturingHandler records every slog.Record it handles so a test can assert on
// the exact log lines WriteError emits. Tests inspect the structured record
// directly, so assertions are immune to any handler's serialization quirks.
type capturingHandler struct {
	mu      *sync.Mutex
	records *[]slog.Record
}

func (h capturingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h capturingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	*h.records = append(*h.records, r.Clone()) // Clone: attrs may share backing storage.
	return nil
}

func (h capturingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h capturingHandler) WithGroup(string) slog.Handler      { return h }

// captureLogs swaps the default slog logger for one that records every line,
// restoring the previous logger on cleanup. WriteError logs via the package-level
// slog functions, which target the default logger — so callers MUST run serially
// (no t.Parallel), since the default logger is process-global.
func captureLogs(t *testing.T) *[]slog.Record {
	t.Helper()

	records := &[]slog.Record{}
	prev := slog.Default()
	slog.SetDefault(slog.New(capturingHandler{mu: &sync.Mutex{}, records: records}))
	t.Cleanup(func() { slog.SetDefault(prev) })

	return records
}

// attrValue returns the value of the named attribute on a record, or ok=false.
func attrValue(r slog.Record, key string) (slog.Value, bool) {
	var (
		val   slog.Value
		found bool
	)
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			val, found = a.Value, true
			return false
		}
		return true
	})
	return val, found
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
		{
			name:       "unavailable",
			err:        apperr.NewUnavailable("provedor indisponível", nil),
			wantStatus: http.StatusServiceUnavailable,
			wantKind:   string(apperr.KindUnavailable),
		},
		{
			name:       "rate limited",
			err:        apperr.NewRateLimited("too many requests"),
			wantStatus: http.StatusTooManyRequests,
			wantKind:   string(apperr.KindRateLimited),
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

func TestWriteError_FiberNotFoundIsClean404(t *testing.T) {
	t.Parallel()

	// Fiber's router hands an unmatched route straight to the ErrorHandler as a
	// *fiber.Error{404,"Cannot GET /x"} (the bot drip on /.well-known/*). It must
	// surface as a clean 404, NOT collapse into an INFRA 500 "internal error".
	status, body, _ := runHandler(t, func(c *fiber.Ctx) error {
		return httpx.WriteError(c, fiber.NewError(fiber.StatusNotFound, "Cannot GET /.well-known/agent.json"))
	})

	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", status)
	}
	if body.Kind != string(apperr.KindNotFound) {
		t.Errorf("kind = %q, want %q", body.Kind, apperr.KindNotFound)
	}
	if body.Message == "internal error" {
		t.Errorf("message = %q, must not be the generic 5xx message", body.Message)
	}
}

func TestWriteError_UnavailableHidesUpstreamCause(t *testing.T) {
	t.Parallel()

	// 503 is a 5xx: the upstream cause is logged, never sent. The client gets the
	// Kind (so it can retry) and a generic message — no raw provider detail leaks.
	const upstream = "brasilapi 500: internal upstream trace"

	status, body, raw := runHandler(t, func(c *fiber.Ctx) error {
		return httpx.WriteError(c, apperr.NewUnavailable("registry unavailable", errors.New(upstream)))
	})

	if status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", status)
	}
	if body.Kind != string(apperr.KindUnavailable) {
		t.Errorf("kind = %q, want %q", body.Kind, apperr.KindUnavailable)
	}
	if body.Message != "internal error" {
		t.Errorf("message = %q, want generic %q", body.Message, "internal error")
	}
	if strings.Contains(raw, "upstream trace") || strings.Contains(raw, "brasilapi") {
		t.Errorf("upstream cause leaked into body: %s", raw)
	}
}

// TestWriteError_Warns4xx guards AC1: the very bug that motivated this slice —
// the Clerk webhook rejecting a bad svix signature with a 401 — used to leave no
// trace. It MUST now emit exactly one Warn line carrying status, kind, message
// and cause, while the client-facing response stays byte-for-byte the same.
func TestWriteError_Warns4xx(t *testing.T) {
	records := captureLogs(t)

	status, body, _ := runHandler(t, func(c *fiber.Ctx) error {
		return httpx.WriteError(c, apperr.NewUnauthorized("invalid signature"))
	})

	// Client-facing behavior is unchanged: same status, same envelope.
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
	if body.Kind != string(apperr.KindUnauthorized) {
		t.Errorf("body kind = %q, want %q", body.Kind, apperr.KindUnauthorized)
	}
	if body.Message != "invalid signature" {
		t.Errorf("body message = %q, want %q", body.Message, "invalid signature")
	}

	if len(*records) != 1 {
		t.Fatalf("emitted %d log lines, want exactly 1", len(*records))
	}
	rec := (*records)[0]
	if rec.Level != slog.LevelWarn {
		t.Errorf("level = %v, want WARN", rec.Level)
	}

	if v, ok := attrValue(rec, "status"); !ok || v.Int64() != int64(http.StatusUnauthorized) {
		t.Errorf("status attr = %v (ok=%v), want 401", v, ok)
	}
	if v, ok := attrValue(rec, "kind"); !ok || v.String() != string(apperr.KindUnauthorized) {
		t.Errorf("kind attr = %v (ok=%v), want %q", v, ok, apperr.KindUnauthorized)
	}
	if v, ok := attrValue(rec, "message"); !ok || v.String() != "invalid signature" {
		t.Errorf("message attr = %v (ok=%v), want %q", v, ok, "invalid signature")
	}

	// cause comes from causeOf(ae): with no wrapped cause it is the AppError
	// itself, whose Error() names the kind and message — enough to diagnose.
	v, ok := attrValue(rec, "cause")
	if !ok {
		t.Fatalf("cause attr missing")
	}
	cause, ok := v.Any().(error)
	if !ok {
		t.Fatalf("cause attr = %v, want an error value", v.Any())
	}
	if !strings.Contains(cause.Error(), "invalid signature") {
		t.Errorf("cause = %q, want it to mention the signature failure", cause.Error())
	}
}

// TestWriteError_5xxStillLogsError guards AC2: adding the 4xx Warn must not
// regress the 5xx branch — it keeps logging at Error with kind and the wrapped
// cause.
func TestWriteError_5xxStillLogsError(t *testing.T) {
	records := captureLogs(t)

	const upstream = "svix verify: connection refused"
	status, _, _ := runHandler(t, func(c *fiber.Ctx) error {
		return httpx.WriteError(c, apperr.NewInfra("boom", errors.New(upstream)))
	})

	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", status)
	}
	if len(*records) != 1 {
		t.Fatalf("emitted %d log lines, want exactly 1", len(*records))
	}
	rec := (*records)[0]
	if rec.Level != slog.LevelError {
		t.Errorf("level = %v, want ERROR (5xx must not regress to Warn)", rec.Level)
	}
	if v, ok := attrValue(rec, "kind"); !ok || v.String() != string(apperr.KindInfra) {
		t.Errorf("kind attr = %v (ok=%v), want %q", v, ok, apperr.KindInfra)
	}
	v, ok := attrValue(rec, "cause")
	if !ok {
		t.Fatalf("cause attr missing on 5xx log")
	}
	cause, _ := v.Any().(error)
	if cause == nil || !strings.Contains(cause.Error(), upstream) {
		t.Errorf("cause = %v, want it to carry %q", v.Any(), upstream)
	}
}

// TestWriteError_SuccessDoesNotLog guards AC3: the success path (2xx) emits no
// log line at all — WriteError only fires on failure.
func TestWriteError_SuccessDoesNotLog(t *testing.T) {
	records := captureLogs(t)

	status, _, _ := runHandler(t, func(c *fiber.Ctx) error {
		return c.Status(http.StatusOK).JSON(fiber.Map{"ok": true})
	})

	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if len(*records) != 0 {
		t.Fatalf("success path emitted %d log lines, want 0", len(*records))
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

// TestWriteValidationError_DoesNotLog locks the chosen behavior: only WriteError
// logs 4xx. WriteValidationError's validation path writes straight to the client
// without a Warn, so high-volume, expected client input mistakes don't drown the
// signal we actually added (auth/svix/malformed-payload failures). Its fallback
// path still logs, because it delegates to WriteError.
func TestWriteValidationError_DoesNotLog(t *testing.T) {
	records := captureLogs(t)

	verr := sampleReq{}.validate()
	if verr == nil {
		t.Fatal("expected validation to fail on empty title")
	}

	status, _, _ := runHandler(t, func(c *fiber.Ctx) error {
		return httpx.WriteValidationError(c, verr)
	})

	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
	if len(*records) != 0 {
		t.Fatalf("validation path emitted %d log lines, want 0", len(*records))
	}
}

// TestWriteValidationError_WrappedIsUnwrapped guards B1: a validation.Errors
// hidden one layer down the chain must still be reached via errors.As, not lost
// to a bare type assertion (which would misclassify it as a 500 INFRA error).
func TestWriteValidationError_WrappedIsUnwrapped(t *testing.T) {
	t.Parallel()

	verr := sampleReq{}.validate()
	if verr == nil {
		t.Fatal("expected validation to fail on empty title")
	}
	wrapped := fmt.Errorf("bind request: %w", verr)

	status, body, _ := runHandler(t, func(c *fiber.Ctx) error {
		return httpx.WriteValidationError(c, wrapped)
	})

	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
	if body.Kind != string(apperr.KindInvalid) {
		t.Errorf("kind = %q, want %q", body.Kind, apperr.KindInvalid)
	}
	if _, ok := body.Details["title"]; !ok {
		t.Errorf("details = %v, want a message under the \"title\" field", body.Details)
	}
}

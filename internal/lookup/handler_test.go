package lookup

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/jusassessoria/platform/lib/apperr"
)

// fakeRegistry is a RegistryLookup double: it returns canned values and records
// whether it was called, so a test can prove validation short-circuits the fetch.
type fakeRegistry struct {
	company Company
	address Address
	err     error
	calls   int
}

func (f *fakeRegistry) LookupCNPJ(context.Context, string) (Company, error) {
	f.calls++
	return f.company, f.err
}

func (f *fakeRegistry) LookupCEP(context.Context, string) (Address, error) {
	f.calls++
	return f.address, f.err
}

// serve wires a handler over reg onto a fresh app (no auth — the router test
// covers AuthUser) and drives one GET, returning status and raw body.
func serve(t *testing.T, reg RegistryLookup, target string) (int, []byte) {
	t.Helper()

	app := fiber.New()
	NewHandler(reg).Register(app)

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, target, nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

func TestHandler_LookupCNPJ_OK(t *testing.T) {
	t.Parallel()

	reg := &fakeRegistry{company: Company{
		CNPJ:      "19131243000197",
		LegalName: "OPEN KNOWLEDGE BRASIL",
		TradeName: "REDE PELO CONHECIMENTO LIVRE",
		Address:   Address{CEP: "01311902", Street: "PAULISTA", Number: "37", City: "SAO PAULO", State: "SP"},
	}}

	// Bare digits: a CNPJ mask carries a '/', which cannot live in a single path
	// segment — the front end sends digits. (Mask normalization is covered by
	// TestNormalizeCNPJ.) The dot-masked CEP below still exercises normalization.
	status, raw := serve(t, reg, "/lookup/cnpj/19131243000197")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", status, raw)
	}

	var got Company
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode body %s: %v", raw, err)
	}
	if got != reg.company {
		t.Errorf("company = %+v, want %+v", got, reg.company)
	}
}

func TestHandler_LookupCEP_OK(t *testing.T) {
	t.Parallel()

	reg := &fakeRegistry{address: Address{
		CEP: "01311902", Street: "Avenida Paulista", Neighborhood: "Bela Vista", City: "São Paulo", State: "SP",
	}}

	status, raw := serve(t, reg, "/lookup/cep/01311-902")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", status, raw)
	}

	var got Address
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode body %s: %v", raw, err)
	}
	if got != reg.address {
		t.Errorf("address = %+v, want %+v", got, reg.address)
	}
}

func TestHandler_ValidationShortCircuitsFetch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		target string
	}{
		{name: "cnpj too short", target: "/lookup/cnpj/123"},
		{name: "cnpj with letters", target: "/lookup/cnpj/1913124300019X"},
		{name: "cep too short", target: "/lookup/cep/123"},
		{name: "cep with letters", target: "/lookup/cep/0131190X"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reg := &fakeRegistry{}
			status, raw := serve(t, reg, tt.target)

			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", status, raw)
			}
			if kind := decodeKind(t, raw); kind != string(apperr.KindInvalid) {
				t.Errorf("kind = %q, want %q", kind, apperr.KindInvalid)
			}
			// The provider must never be hit for a malformed id.
			if reg.calls != 0 {
				t.Errorf("registry called %d times, want 0 (validation runs first)", reg.calls)
			}
		})
	}
}

func TestHandler_PropagatesRegistryErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		target     string
		regErr     error
		wantStatus int
		wantKind   string
	}{
		{
			name:       "cnpj not found",
			target:     "/lookup/cnpj/19131243000197",
			regErr:     ErrNotFound,
			wantStatus: http.StatusNotFound,
			wantKind:   string(apperr.KindNotFound),
		},
		{
			name:       "cnpj unavailable",
			target:     "/lookup/cnpj/19131243000197",
			regErr:     apperr.NewUnavailable("provider down", nil),
			wantStatus: http.StatusServiceUnavailable,
			wantKind:   string(apperr.KindUnavailable),
		},
		{
			name:       "cep not found",
			target:     "/lookup/cep/01311902",
			regErr:     ErrNotFound,
			wantStatus: http.StatusNotFound,
			wantKind:   string(apperr.KindNotFound),
		},
		{
			name:       "cep unavailable",
			target:     "/lookup/cep/01311902",
			regErr:     apperr.NewUnavailable("provider down", nil),
			wantStatus: http.StatusServiceUnavailable,
			wantKind:   string(apperr.KindUnavailable),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reg := &fakeRegistry{err: tt.regErr}
			status, raw := serve(t, reg, tt.target)

			if status != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", status, tt.wantStatus, raw)
			}
			if kind := decodeKind(t, raw); kind != tt.wantKind {
				t.Errorf("kind = %q, want %q", kind, tt.wantKind)
			}
		})
	}
}

// decodeKind reads the {kind,...} error envelope from a raw payload.
func decodeKind(t *testing.T, raw []byte) string {
	t.Helper()
	var env struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode error envelope %s: %v", raw, err)
	}
	return env.Kind
}

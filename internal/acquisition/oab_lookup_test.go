package acquisition

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// djenComunicacaoWithAdvogado builds a minimal comunicação JSON naming one
// advogado — enough to exercise LookupOABName's match/extract logic.
func djenComunicacaoWithAdvogado(hash, nome, oab, uf string) json.RawMessage {
	raw, _ := json.Marshal(djenComunicacao{
		Hash:                 hash,
		NumeroProcesso:       "1",
		DataDisponibilizacao: "2026-08-01",
		Advogados: []djenAdvogadoLink{
			{Advogado: djenAdvogado{Nome: nome, NumeroOAB: oab, UFOAB: uf}},
		},
	})
	return raw
}

// TestLookupOABName_Found asserts a single-page request extracts the name of the
// matching OAB from destinatarioadvogados, and the query carries the expected
// window + oab params (no multi-page walk — this is the on-demand path).
func TestLookupOABName_Found(t *testing.T) {
	t.Parallel()

	var gotQuery map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		gotQuery = map[string]string{
			"numeroOab":      q.Get("numeroOab"),
			"ufOab":          q.Get("ufOab"),
			"pagina":         q.Get("pagina"),
			"itensPorPagina": q.Get("itensPorPagina"),
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(djenBody(t,
			djenComunicacaoWithAdvogado("h1", "OUTRO ADVOGADO", "999999", "RJ"),
			djenComunicacaoWithAdvogado("h2", "LUAN GOMES", "347019", "SP"),
		))
	}))
	defer srv.Close()

	c := NewDJENConnector(WithDJENBaseURL(srv.URL), WithDJENRatePerMinute(6000000))
	name, err := c.LookupOABName(context.Background(), OABEntry{Number: "347019", UF: "SP"})
	if err != nil {
		t.Fatalf("LookupOABName: %v", err)
	}
	if name != "LUAN GOMES" {
		t.Errorf("name = %q, want %q", name, "LUAN GOMES")
	}
	if gotQuery["numeroOab"] != "347019" || gotQuery["ufOab"] != "SP" {
		t.Errorf("query oab = %s/%s, want 347019/SP", gotQuery["numeroOab"], gotQuery["ufOab"])
	}
	if gotQuery["pagina"] != "1" {
		t.Errorf("query pagina = %s, want 1 (single page, no walk)", gotQuery["pagina"])
	}
}

// TestLookupOABName_NotFound asserts a page with no matching OAB (or no items at
// all) surfaces the typed ErrOABNotFound — the caller's expected "nothing to
// auto-fill" signal, not a fault.
func TestLookupOABName_NotFound(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(djenBody(t, djenComunicacaoWithAdvogado("h1", "OUTRO", "999999", "RJ")))
	}))
	defer srv.Close()

	c := NewDJENConnector(WithDJENBaseURL(srv.URL), WithDJENRatePerMinute(6000000))
	_, err := c.LookupOABName(context.Background(), OABEntry{Number: "347019", UF: "SP"})
	if !errors.Is(err, ErrOABNotFound) {
		t.Fatalf("err = %v, want ErrOABNotFound", err)
	}
}

// TestParseOAB covers the boundary rule: UF (2 letters) + 1-6 digits, split into
// the OABEntry the connector queries by.
func TestParseOAB(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     string
		want    OABEntry
		wantErr bool
	}{
		{name: "valid", raw: "SP123456", want: OABEntry{UF: "SP", Number: "123456"}},
		{name: "valid short number", raw: "MG1", want: OABEntry{UF: "MG", Number: "1"}},
		{name: "lowercase uf is invalid", raw: "sp123456", wantErr: true},
		{name: "missing digits is invalid", raw: "SP", wantErr: true},
		{name: "too many digits is invalid", raw: "SP1234567", wantErr: true},
		{name: "empty is invalid", raw: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseOAB(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseOAB(%q) = nil error, want one", tt.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseOAB(%q) error = %v, want nil", tt.raw, err)
			}
			if got != tt.want {
				t.Errorf("parseOAB(%q) = %+v, want %+v", tt.raw, got, tt.want)
			}
		})
	}
}

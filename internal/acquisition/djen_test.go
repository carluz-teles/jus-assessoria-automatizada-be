package acquisition

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// djenItem builds a minimal comunicação JSON with a given hash — enough for the
// connector's dedup peek (the parser's field mapping is exercised elsewhere).
func djenItem(hash string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"hash":%q,"numero_processo":"1","tipoComunicacao":"Intimação"}`, hash))
}

func djenEnvelope(t *testing.T, items ...json.RawMessage) []byte {
	t.Helper()
	body, err := json.Marshal(djenResponse{Status: djenStatusSuccess, Count: len(items), Items: items})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return body
}

// TestWithDJENProxy asserts the proxy option routes the connector's outbound
// requests through the configured proxy — the WAF egress-IP fix. The client's
// Transport resolver must return the proxy URL for any request.
func TestWithDJENProxy(t *testing.T) {
	t.Parallel()

	proxyURL, err := url.Parse("http://user:pass@proxy.br.example:8080")
	if err != nil {
		t.Fatalf("parse proxy url: %v", err)
	}

	c := NewDJENConnector(WithDJENProxy(proxyURL))

	transport, ok := c.httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("httpClient.Transport = %T, want *http.Transport", c.httpClient.Transport)
	}

	req := httptest.NewRequest(http.MethodGet, "https://comunicaapi.pje.jus.br/api/v1/comunicacao", nil)
	got, err := transport.Proxy(req)
	if err != nil {
		t.Fatalf("Proxy resolver: %v", err)
	}
	if got == nil || got.String() != proxyURL.String() {
		t.Errorf("proxy = %v, want %v", got, proxyURL)
	}
}

// TestWithDJENProxy_Nil keeps the direct connection (nil Transport → the client
// falls back to http.DefaultTransport) when no proxy is configured.
func TestWithDJENProxy_Nil(t *testing.T) {
	t.Parallel()

	c := NewDJENConnector(WithDJENProxy(nil))
	if c.httpClient.Transport != nil {
		t.Errorf("Transport = %v, want nil (direct connection)", c.httpClient.Transport)
	}
}

// TestDJENConnectorFetch walks two OABs with pagination and cross-OAB overlap and
// asserts the connector sends the right consulta params, paginates until a short
// page, dedups by hash, and tags the payload as DJEN.
func TestDJENConnectorFetch(t *testing.T) {
	t.Parallel()

	var gotParams []map[string]string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		gotParams = append(gotParams, map[string]string{
			"numeroOab":      q.Get("numeroOab"),
			"ufOab":          q.Get("ufOab"),
			"inicio":         q.Get("dataDisponibilizacaoInicio"),
			"fim":            q.Get("dataDisponibilizacaoFim"),
			"pagina":         q.Get("pagina"),
			"itensPorPagina": q.Get("itensPorPagina"),
		})

		w.Header().Set("Content-Type", "application/json")
		switch {
		case q.Get("numeroOab") == "111" && q.Get("pagina") == "1":
			// full page (== pageSize) → the walk continues to page 2
			_, _ = w.Write(djenEnvelope(t, djenItem("X"), djenItem("Y")))
		case q.Get("numeroOab") == "111" && q.Get("pagina") == "2":
			// short page → the walk stops
			_, _ = w.Write(djenEnvelope(t, djenItem("Z")))
		case q.Get("numeroOab") == "222" && q.Get("pagina") == "1":
			// "Y" overlaps OAB 111 (same process, both advogados) → deduped
			_, _ = w.Write(djenEnvelope(t, djenItem("Y")))
		default:
			_, _ = w.Write(djenEnvelope(t))
		}
	}))
	defer srv.Close()

	c := NewDJENConnector(WithDJENBaseURL(srv.URL), WithDJENRatePerMinute(6000000), WithDJENPageSize(2))
	raw, err := c.Fetch(context.Background(), FetchRequest{
		Capability: CapabilityDiscoverByOAB,
		WindowFrom: "2024-01-01",
		WindowTo:   "2024-01-08",
		OABs:       []OABEntry{{Number: "111", UF: "SP"}, {Number: "222", UF: "SP"}},
	})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	if raw.Source != SourceDJEN || raw.ConnectorID != djenConnectorID {
		t.Errorf("payload tags = (%s, %s), want (%s, %s)", raw.Source, raw.ConnectorID, SourceDJEN, djenConnectorID)
	}

	var payload djenPayload
	if err := json.Unmarshal(raw.Body, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if len(payload.OABs) != 2 {
		t.Errorf("payload OABs = %d, want 2", len(payload.OABs))
	}
	if len(payload.Items) != 3 { // X, Y, Z — the second Y deduped
		t.Fatalf("payload items = %d, want 3 (deduped)", len(payload.Items))
	}

	// First request carries the window and OAB verbatim.
	first := gotParams[0]
	if first["numeroOab"] != "111" || first["ufOab"] != "SP" {
		t.Errorf("first request oab = %s/%s, want 111/SP", first["numeroOab"], first["ufOab"])
	}
	if first["inicio"] != "2024-01-01" || first["fim"] != "2024-01-08" {
		t.Errorf("first request window = %s..%s, want 2024-01-01..2024-01-08", first["inicio"], first["fim"])
	}
	if first["itensPorPagina"] != "2" {
		t.Errorf("first request itensPorPagina = %s, want 2", first["itensPorPagina"])
	}
}

// TestDJENConnectorFetchErrors covers the non-happy paths: an unsupported
// capability short-circuits without any HTTP call, and a non-success envelope or a
// non-200 status surfaces as a (retryable) fetch error.
func TestDJENConnectorFetchErrors(t *testing.T) {
	t.Parallel()

	t.Run("unsupported capability", func(t *testing.T) {
		t.Parallel()
		c := NewDJENConnector(WithDJENBaseURL("http://unused.invalid"))
		_, err := c.Fetch(context.Background(), FetchRequest{
			Capability: CapabilityFetchByNumber,
			OABs:       []OABEntry{{Number: "1", UF: "SP"}},
		})
		if err == nil {
			t.Fatal("want error for unsupported capability, got nil")
		}
	})

	t.Run("error envelope is a fetch fault", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"status":"error","message":"parâmetro obrigatório ausente"}`))
		}))
		defer srv.Close()

		c := NewDJENConnector(WithDJENBaseURL(srv.URL), WithDJENRatePerMinute(6000000))
		_, err := c.Fetch(context.Background(), FetchRequest{
			Capability: CapabilityDiscoverByOAB,
			OABs:       []OABEntry{{Number: "1", UF: "SP"}},
		})
		if err == nil {
			t.Fatal("want error for non-success envelope, got nil")
		}
	})

	t.Run("non-200 is a fetch fault", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		}))
		defer srv.Close()

		c := NewDJENConnector(WithDJENBaseURL(srv.URL), WithDJENRatePerMinute(6000000))
		_, err := c.Fetch(context.Background(), FetchRequest{
			Capability: CapabilityDiscoverByOAB,
			OABs:       []OABEntry{{Number: "1", UF: "SP"}},
		})
		if err == nil {
			t.Fatal("want error for HTTP 403, got nil")
		}
	})
}

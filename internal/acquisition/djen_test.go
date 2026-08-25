package acquisition

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/obs"
)

// djenItem builds a minimal comunicação JSON with a given hash — enough for the
// connector's dedup peek (the parser's field mapping is exercised elsewhere).
func djenItem(hash string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"hash":%q,"numero_processo":"1","tipoComunicacao":"Intimação"}`, hash))
}

func djenBody(t *testing.T, items ...json.RawMessage) []byte {
	t.Helper()
	body, err := json.Marshal(djenResponse{Status: djenStatusSuccess, Count: len(items), Items: items})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return body
}

// TestWithDJENProxy asserts the proxy option routes the connector's outbound
// requests through the configured proxy — the WAF egress-IP fix. The client's
// WithDJENProxy records the CONNECT proxy on the connector; the uTLS transport tunnels
// through it (the proxy is applied inside DialTLSContext, not Transport.Proxy, so the
// Chrome handshake runs over the tunnel). The transport is always the uTLS one.
func TestWithDJENProxy(t *testing.T) {
	t.Parallel()

	proxyURL, err := url.Parse("http://user:pass@proxy.br.example:8080")
	if err != nil {
		t.Fatalf("parse proxy url: %v", err)
	}

	c := NewDJENConnector(WithDJENProxy(proxyURL))

	if c.proxyURL == nil || c.proxyURL.String() != proxyURL.String() {
		t.Errorf("proxyURL = %v, want %v", c.proxyURL, proxyURL)
	}
	transport, ok := c.httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("httpClient.Transport = %T, want *http.Transport", c.httpClient.Transport)
	}
	if transport.DialTLSContext == nil {
		t.Error("DialTLSContext = nil, want the uTLS Chrome dialer")
	}
	// The Proxy field is intentionally unused: the tunnel is opened inside DialTLSContext.
	if transport.Proxy != nil {
		t.Error("Transport.Proxy set, want nil (CONNECT handled in DialTLSContext)")
	}
}

// WithDJENProxy(nil) keeps the direct connection, but the transport is STILL the uTLS
// one — the Chrome fingerprint is needed with or without the proxy (the JA3 throttle is
// independent of the egress IP).
func TestWithDJENProxy_Nil(t *testing.T) {
	t.Parallel()

	c := NewDJENConnector(WithDJENProxy(nil))
	if c.proxyURL != nil {
		t.Errorf("proxyURL = %v, want nil (direct)", c.proxyURL)
	}
	transport, ok := c.httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("httpClient.Transport = %T, want *http.Transport (uTLS)", c.httpClient.Transport)
	}
	if transport.DialTLSContext == nil {
		t.Error("DialTLSContext = nil, want the uTLS Chrome dialer even without a proxy")
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
			_, _ = w.Write(djenBody(t, djenItem("X"), djenItem("Y")))
		case q.Get("numeroOab") == "111" && q.Get("pagina") == "2":
			// short page → the walk stops
			_, _ = w.Write(djenBody(t, djenItem("Z")))
		case q.Get("numeroOab") == "222" && q.Get("pagina") == "1":
			// "Y" overlaps OAB 111 (same process, both advogados) → deduped
			_, _ = w.Write(djenBody(t, djenItem("Y")))
		default:
			_, _ = w.Write(djenBody(t))
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

// A 429 is captured on the djen.request_page span: the HTTP status, the rate_limited
// flag and the typed error.kind (SERVICE_UNAVAILABLE, stamped by obs.Record) — so the
// backend can facet a rate block apart from a bug without parsing the message. NOT
// parallel: it installs a global recorder, which the package's parallel tests (parked
// meanwhile) must not observe.
func TestDJENConnector_RequestPageSpan(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev); _ = tp.Shutdown(context.Background()) })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := NewDJENConnector(WithDJENBaseURL(srv.URL), WithDJENRatePerMinute(6000000))
	if _, err := c.Fetch(context.Background(), FetchRequest{
		Capability: CapabilityDiscoverByOAB,
		OABs:       []OABEntry{{Number: "347019", UF: "SP"}},
	}); err == nil {
		t.Fatal("want error for HTTP 429, got nil")
	}

	span := findSpan(t, sr.Ended(), "djen.request_page")
	attrs := attrMap(span.Attributes())
	if got := attrs["http.response.status_code"]; got != int64(http.StatusTooManyRequests) {
		t.Errorf("status attr = %v, want 429", got)
	}
	if got := attrs["djen.rate_limited"]; got != true {
		t.Errorf("rate_limited attr = %v, want true", got)
	}
	if got := attrs[obs.KeyErrorKind]; got != string(apperr.KindUnavailable) {
		t.Errorf("error.kind attr = %v, want %s", got, apperr.KindUnavailable)
	}
	if span.Status().Code != codes.Error {
		t.Errorf("span status = %v, want Error", span.Status().Code)
	}
}

// findSpan returns the one ended span with the given name, failing if absent.
func findSpan(t *testing.T, spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, s := range spans {
		if s.Name() == name {
			return s
		}
	}
	t.Fatalf("span %q not recorded (got %d spans)", name, len(spans))
	return nil
}

// attrMap flattens span attributes to a comparable map keyed by attribute name.
func attrMap(attrs []attribute.KeyValue) map[string]any {
	m := make(map[string]any, len(attrs))
	for _, a := range attrs {
		m[string(a.Key)] = a.Value.AsInterface()
	}
	return m
}

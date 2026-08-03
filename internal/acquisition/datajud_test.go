package acquisition

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDATAJUDConnectorFetch asserts the connector targets the right per-tribunal
// index, sends the APIKey and the numeroProcesso query, and tags the raw ES
// envelope as DATAJUD.
func TestDATAJUDConnectorFetch(t *testing.T) {
	t.Parallel()

	var gotPath, gotAuth, gotBody, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"hits":{"total":{"value":1},"hits":[{"_source":{"grau":"G1"}}]}}`))
	}))
	defer srv.Close()

	c := NewDATAJUDConnector(WithDATAJUDBaseURL(srv.URL), WithDATAJUDAPIKey("test-key"))
	raw, err := c.Fetch(context.Background(), FetchRequest{
		Capability: CapabilityFetchByNumber,
		CNJNumber:  "50007978720168210156",
		Court:      "TJRS",
	})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if gotPath != "/api_publica_tjrs/_search" {
		t.Errorf("path = %s, want /api_publica_tjrs/_search", gotPath)
	}
	if gotAuth != "APIKey test-key" {
		t.Errorf("authorization = %q, want APIKey test-key", gotAuth)
	}
	if !strings.Contains(gotBody, `"numeroProcesso":"50007978720168210156"`) {
		t.Errorf("body = %s, want it to match numeroProcesso", gotBody)
	}
	if raw.Source != SourceDATAJUD || raw.ConnectorID != datajudConnectorID {
		t.Errorf("payload tags = (%s, %s), want (%s, %s)", raw.Source, raw.ConnectorID, SourceDATAJUD, datajudConnectorID)
	}
	if !strings.Contains(string(raw.Body), `"grau":"G1"`) {
		t.Errorf("body not carried through: %s", raw.Body)
	}
}

func TestDATAJUDConnectorFetchErrors(t *testing.T) {
	t.Parallel()

	t.Run("unsupported capability", func(t *testing.T) {
		t.Parallel()
		c := NewDATAJUDConnector(WithDATAJUDBaseURL("http://unused.invalid"))
		_, err := c.Fetch(context.Background(), FetchRequest{Capability: CapabilityDiscoverByOAB, CNJNumber: "1", Court: "TJSP"})
		if err == nil {
			t.Fatal("want error for unsupported capability")
		}
	})

	t.Run("missing court or number", func(t *testing.T) {
		t.Parallel()
		c := NewDATAJUDConnector(WithDATAJUDBaseURL("http://unused.invalid"))
		if _, err := c.Fetch(context.Background(), FetchRequest{Capability: CapabilityFetchByNumber, CNJNumber: "1"}); err == nil {
			t.Error("want error when court is empty")
		}
		if _, err := c.Fetch(context.Background(), FetchRequest{Capability: CapabilityFetchByNumber, Court: "TJSP"}); err == nil {
			t.Error("want error when number is empty")
		}
	})

	t.Run("non-200 is a fetch fault", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		defer srv.Close()

		c := NewDATAJUDConnector(WithDATAJUDBaseURL(srv.URL))
		_, err := c.Fetch(context.Background(), FetchRequest{Capability: CapabilityFetchByNumber, CNJNumber: "1", Court: "TJSP"})
		if err == nil {
			t.Fatal("want error for HTTP 429")
		}
	})
}

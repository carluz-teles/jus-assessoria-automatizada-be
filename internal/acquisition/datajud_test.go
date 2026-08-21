package acquisition

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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

// TestDATAJUDConnectorFetchBatch_TermsOneRequest asserts a small batch (< cap, short page)
// costs ONE request, sends a `terms` query on the PLAIN numeroProcesso field, and carries
// every hit's payload through. A requested number with no hit is simply absent (not an error).
func TestDATAJUDConnectorFetchBatch_TermsOneRequest(t *testing.T) {
	t.Parallel()

	var requests atomic.Int64
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		// Two of the three requested numbers come back; the third is simply absent.
		_, _ = w.Write([]byte(`{"hits":{"hits":[
			{"_source":{"numeroProcesso":"111","grau":"G1","tribunal":"TJSP"},"sort":["111"]},
			{"_source":{"numeroProcesso":"222","grau":"G2","tribunal":"TJSP"},"sort":["222"]}
		]}}`))
	}))
	defer srv.Close()

	c := NewDATAJUDConnector(WithDATAJUDBaseURL(srv.URL), WithDATAJUDRatePerMinute(6000))
	pages, err := c.FetchBatch(context.Background(), FetchRequest{
		Capability: CapabilityFetchBatch,
		Court:      "TJSP",
		CNJNumbers: []string{"111", "222", "333"},
	})
	if err != nil {
		t.Fatalf("FetchBatch: %v", err)
	}
	if requests.Load() != 1 {
		t.Errorf("requests = %d, want 1 (short page ends pagination)", requests.Load())
	}
	if len(pages) != 1 {
		t.Fatalf("pages = %d, want 1", len(pages))
	}
	// PLAIN field terms (not .keyword) with all requested numbers.
	if !strings.Contains(gotBody, `"terms":{"numeroProcesso":["111","222","333"]}`) {
		t.Errorf("body = %s, want a terms query on the plain numeroProcesso field", gotBody)
	}
	if !strings.Contains(string(pages[0].Body), `"numeroProcesso":"111"`) || !strings.Contains(string(pages[0].Body), `"numeroProcesso":"222"`) {
		t.Errorf("page body missing hits: %s", pages[0].Body)
	}
}

// TestDATAJUDConnectorFetchBatch_PaginatesFullPage asserts a FULL page (hits == size)
// triggers a search_after page, and a following short page ends it — one Wait per request.
func TestDATAJUDConnectorFetchBatch_PaginatesFullPage(t *testing.T) {
	t.Parallel()

	var requests atomic.Int64
	var sawSearchAfter atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		var body datajudTermsSearch
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			// FULL page (size == 2 for a 2-number request) → the connector must page on.
			_, _ = w.Write([]byte(`{"hits":{"hits":[
				{"_source":{"numeroProcesso":"111","grau":"G1","tribunal":"TJSP"},"sort":["111"]},
				{"_source":{"numeroProcesso":"222","grau":"G1","tribunal":"TJSP"},"sort":["222"]}
			]}}`))
			return
		}
		// The 2nd request must carry search_after = last sort token of page 1.
		if len(body.SearchAfter) == 1 {
			sawSearchAfter.Store(true)
		}
		_, _ = w.Write([]byte(`{"hits":{"hits":[
			{"_source":{"numeroProcesso":"333","grau":"G1","tribunal":"TJSP"},"sort":["333"]}
		]}}`))
	}))
	defer srv.Close()

	c := NewDATAJUDConnector(WithDATAJUDBaseURL(srv.URL), WithDATAJUDRatePerMinute(6000))
	pages, err := c.FetchBatch(context.Background(), FetchRequest{
		Capability: CapabilityFetchBatch,
		Court:      "TJSP",
		CNJNumbers: []string{"111", "222"}, // size = 2; page 1 comes back FULL → paginate
	})
	if err != nil {
		t.Fatalf("FetchBatch: %v", err)
	}
	if requests.Load() != 2 {
		t.Errorf("requests = %d, want 2 (full page → one search_after page)", requests.Load())
	}
	if !sawSearchAfter.Load() {
		t.Error("2nd request did not carry search_after token")
	}
	if len(pages) != 2 {
		t.Errorf("pages = %d, want 2", len(pages))
	}
}

// TestDATAJUDConnectorFetchBatchErrors covers the guards.
func TestDATAJUDConnectorFetchBatchErrors(t *testing.T) {
	t.Parallel()

	c := NewDATAJUDConnector(WithDATAJUDBaseURL("http://unused.invalid"))
	if _, err := c.FetchBatch(context.Background(), FetchRequest{Capability: CapabilityFetchByNumber, Court: "TJSP", CNJNumbers: []string{"1"}}); err == nil {
		t.Error("want error for wrong capability")
	}
	if _, err := c.FetchBatch(context.Background(), FetchRequest{Capability: CapabilityFetchBatch, CNJNumbers: []string{"1"}}); err == nil {
		t.Error("want error when court is empty")
	}
	if _, err := c.FetchBatch(context.Background(), FetchRequest{Capability: CapabilityFetchBatch, Court: "TJSP"}); err == nil {
		t.Error("want error when numbers is empty")
	}
}

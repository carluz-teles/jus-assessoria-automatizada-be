package acquisition

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// TestDJENConnectorParserPipeline is the component integration for the DJEN slice:
// the REAL connector fetches the captured Comunica API page over HTTP, and the
// ParserSet routes the resulting payload to the DJEN parser, which maps it to the
// domain shape. It ties the two new units together against real data, in-process
// (httptest), so it runs in the plain green gate — the DB-backed end-to-end
// (sync_run/outbox against Postgres) is a later integration-tagged test.
func TestDJENConnectorParserPipeline(t *testing.T) {
	t.Parallel()

	fixture, err := os.ReadFile("testdata/djen_comunicacao_page.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture)
	}))
	defer srv.Close()

	connector := NewDJENConnector(WithDJENBaseURL(srv.URL))
	raw, err := connector.Fetch(context.Background(), FetchRequest{
		Capability: CapabilityDiscoverByOAB,
		WindowFrom: "2026-07-01",
		WindowTo:   "2026-07-31",
		OABs:       []OABEntry{{Number: "41101", UF: "RS"}},
	})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	// The ParserSet dispatches by source: the DJEN parser claims this payload, the
	// stub (DATAJUD) does not.
	parser := ParserSet{NewDJENParser(fakeCalendar{}), NewStubParser()}
	if !parser.CanParse(raw) {
		t.Fatal("ParserSet cannot parse the DJEN payload")
	}
	res, err := parser.Parse(context.Background(), raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if len(res.Intimations) == 0 {
		t.Fatal("pipeline produced no intimations from the captured page")
	}
	for _, in := range res.Intimations {
		if in.Source != SourceDJEN || in.Degree != DegreeUnknown {
			t.Errorf("intimation %s: source=%q degree=%q, want DJEN/UNKNOWN", in.Hash, in.Source, in.Degree)
		}
		if in.PublishedAt.Before(in.MadeAvailableAt) {
			t.Errorf("intimation %s: published_at %s precedes availability %s", in.Hash, in.PublishedAt, in.MadeAvailableAt)
		}
	}
}

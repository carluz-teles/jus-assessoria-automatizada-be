package acquisition

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jusassessoria/platform/lib/apperr"
)

// djenCapture records what a stub Comunica server was asked, so a test can assert
// both the pagination shape (number of GETs) and the querystring it built. The
// mutex keeps it race-clean under `go test -race` even though one Fetch pages
// sequentially.
type djenCapture struct {
	mu      sync.Mutex
	queries []map[string]string
}

func (c *djenCapture) record(q map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.queries = append(c.queries, q)
}

func (c *djenCapture) calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.queries)
}

func (c *djenCapture) snapshot() []map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]map[string]string, len(c.queries))
	copy(out, c.queries)
	return out
}

// newDJENServer serves a Comunica-shaped envelope whose item count for a given
// (numeroOab, pagina) is decided by itemsFor, and captures every query it saw.
func newDJENServer(t *testing.T, itemsFor func(oab string, page int) int) (*httptest.Server, *djenCapture) {
	t.Helper()

	cap := &djenCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		cap.record(map[string]string{
			"numeroOab":                  q.Get("numeroOab"),
			"ufOab":                      q.Get("ufOab"),
			"dataDisponibilizacaoInicio": q.Get("dataDisponibilizacaoInicio"),
			"dataDisponibilizacaoFim":    q.Get("dataDisponibilizacaoFim"),
			"pagina":                     q.Get("pagina"),
			"itensPorPagina":             q.Get("itensPorPagina"),
		})

		page, _ := strconv.Atoi(q.Get("pagina"))
		writeDJENPage(w, itemsFor(q.Get("numeroOab"), page))
	}))
	t.Cleanup(srv.Close)
	return srv, cap
}

// writeDJENPage writes the {status,message,count,items[]} envelope with n items,
// each a small distinct object so the aggregated payload round-trips.
func writeDJENPage(w http.ResponseWriter, n int) {
	items := make([]map[string]int, 0, n)
	for i := range n {
		items = append(items, map[string]int{"id": i})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":  "success",
		"message": "",
		"count":   n,
		"items":   items,
	})
}

// aggregatedItems unmarshals the RawPayload body back into the raw item list so a
// test can count what the connector collected across pages/OABs.
func aggregatedItems(t *testing.T, p RawPayload) []json.RawMessage {
	t.Helper()

	var items []json.RawMessage
	if err := json.Unmarshal(p.Body, &items); err != nil {
		t.Fatalf("unmarshal aggregated body: %v", err)
	}
	return items
}

func oneOAB() []OABEntry { return []OABEntry{{Number: "123456", UF: "SP"}} }

func TestDJENConnector_Capabilities(t *testing.T) {
	t.Parallel()

	c := NewDJENConnector()

	if c.ID() != "djen" {
		t.Errorf("ID() = %q, want djen", c.ID())
	}

	var found bool
	for _, capb := range c.Capabilities() {
		if capb == CapabilityDiscoverByOAB {
			found = true
		}
	}
	if !found {
		t.Errorf("Capabilities() = %v, want to contain %q", c.Capabilities(), CapabilityDiscoverByOAB)
	}
}

func TestDJENConnector_Fetch_PaginatesAndAggregates(t *testing.T) {
	t.Parallel()

	// Page 1 is full (itensPorPagina), so the connector must fetch page 2, which
	// is short and ends the loop. The aggregate is every item from both pages.
	const secondPage = 3
	itemsFor := func(_ string, page int) int {
		if page == 1 {
			return itensPorPagina
		}
		return secondPage
	}
	srv, capture := newDJENServer(t, itemsFor)
	c := NewDJENConnector(WithBaseURL(srv.URL))

	got, err := c.Fetch(context.Background(), FetchRequest{
		Capability: CapabilityDiscoverByOAB,
		WindowFrom: "2024-01-01",
		WindowTo:   "2024-01-31",
		OABs:       oneOAB(),
	})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	if got.ConnectorID != "djen" || got.Source != SourceDJEN {
		t.Errorf("payload identity = %q/%q, want djen/%s", got.ConnectorID, got.Source, SourceDJEN)
	}
	if n := len(aggregatedItems(t, got)); n != itensPorPagina+secondPage {
		t.Errorf("aggregated items = %d, want %d", n, itensPorPagina+secondPage)
	}

	// The querystring must carry the discovery filters the API expects.
	first := capture.snapshot()[0]
	wantFirst := map[string]string{
		"numeroOab":                  "123456",
		"ufOab":                      "SP",
		"dataDisponibilizacaoInicio": "2024-01-01",
		"dataDisponibilizacaoFim":    "2024-01-31",
		"pagina":                     "1",
		"itensPorPagina":             strconv.Itoa(itensPorPagina),
	}
	for k, want := range wantFirst {
		if first[k] != want {
			t.Errorf("query[%q] = %q, want %q", k, first[k], want)
		}
	}
}

func TestDJENConnector_Fetch_StopsOnShortPage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		itemsFor  func(oab string, page int) int
		wantCalls int
		wantItems int
	}{
		{
			name:      "single short page stops after one GET",
			itemsFor:  func(_ string, _ int) int { return 5 },
			wantCalls: 1,
			wantItems: 5,
		},
		{
			name: "full page then short page stops after two GETs",
			itemsFor: func(_ string, page int) int {
				if page == 1 {
					return itensPorPagina
				}
				return 2
			},
			wantCalls: 2,
			wantItems: itensPorPagina + 2,
		},
		{
			name: "two full pages then empty page stops after three GETs",
			itemsFor: func(_ string, page int) int {
				if page <= 2 {
					return itensPorPagina
				}
				return 0
			},
			wantCalls: 3,
			wantItems: 2 * itensPorPagina,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv, capture := newDJENServer(t, tt.itemsFor)
			c := NewDJENConnector(WithBaseURL(srv.URL))

			got, err := c.Fetch(context.Background(), FetchRequest{
				Capability: CapabilityDiscoverByOAB,
				OABs:       oneOAB(),
			})
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}

			if capture.calls() != tt.wantCalls {
				t.Errorf("GET count = %d, want %d", capture.calls(), tt.wantCalls)
			}
			if n := len(aggregatedItems(t, got)); n != tt.wantItems {
				t.Errorf("aggregated items = %d, want %d", n, tt.wantItems)
			}
		})
	}
}

func TestDJENConnector_Fetch_MultipleOABs(t *testing.T) {
	t.Parallel()

	// Two OABs, each a single short page: one GET per OAB, items aggregated across
	// both. The distinct per-page counts let the total pin down that both ran.
	countByOAB := map[string]int{"111": 2, "222": 3}
	itemsFor := func(oab string, _ int) int { return countByOAB[oab] }
	srv, capture := newDJENServer(t, itemsFor)
	c := NewDJENConnector(WithBaseURL(srv.URL))

	got, err := c.Fetch(context.Background(), FetchRequest{
		Capability: CapabilityDiscoverByOAB,
		OABs: []OABEntry{
			{Number: "111", UF: "SP"},
			{Number: "222", UF: "RJ"},
		},
	})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	if capture.calls() != 2 {
		t.Errorf("GET count = %d, want 2 (one per OAB)", capture.calls())
	}
	if n := len(aggregatedItems(t, got)); n != countByOAB["111"]+countByOAB["222"] {
		t.Errorf("aggregated items = %d, want %d", n, countByOAB["111"]+countByOAB["222"])
	}

	queried := map[string]bool{}
	for _, q := range capture.snapshot() {
		queried[q["numeroOab"]] = true
	}
	if !queried["111"] || !queried["222"] {
		t.Errorf("queried OABs = %v, want both 111 and 222", queried)
	}
}

func TestDJENConnector_Fetch_ServerErrorIsUnavailable(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	c := NewDJENConnector(WithBaseURL(srv.URL))

	_, err := c.Fetch(context.Background(), FetchRequest{OABs: oneOAB()})
	assertKind(t, err, apperr.KindUnavailable)
}

func TestDJENConnector_Fetch_TimeoutIsUnavailable(t *testing.T) {
	t.Parallel()

	// The server never answers within the client's timeout. close(block) is
	// deferred before srv.Close so the handler unblocks first and Close does not
	// deadlock on the in-flight request.
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-block
	}))
	defer srv.Close()
	defer close(block)

	c := NewDJENConnector(
		WithBaseURL(srv.URL),
		WithHTTPClient(&http.Client{Timeout: 30 * time.Millisecond}),
	)

	_, err := c.Fetch(context.Background(), FetchRequest{OABs: oneOAB()})
	assertKind(t, err, apperr.KindUnavailable)
}

// assertKind fails unless err carries an *AppError of the wanted kind.
func assertKind(t *testing.T, err error, want apperr.Kind) {
	t.Helper()

	if err == nil {
		t.Fatalf("got nil error, want kind %q", want)
	}
	ae, ok := apperr.From(err)
	if !ok {
		t.Fatalf("error %v is not an *AppError", err)
	}
	if ae.Kind != want {
		t.Fatalf("error kind = %q, want %q", ae.Kind, want)
	}
}

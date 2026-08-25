package acquisition

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/jusassessoria/platform/lib/apperr"
)

// djen_connector.go is the production Connector for the DJEN (Comunica/PJe): it
// discovers a tenant's processes by OAB over a date window, paging the public
// Comunica API and aggregating every item into one RawPayload the parser (a
// later sub-slice) turns into intimations. It replicates lookup/client.go's HTTP
// shape (functional options, a private get(), status→apperr mapping) rather than
// importing that slice — slices never depend on each other's packages.

const (
	// djenConnectorID identifies this connector in the sync_run audit row and on
	// every RawPayload it emits.
	djenConnectorID = "djen"

	// djenConnectorVersion tags the payloads so a re-parse can be attributed to
	// the connector build that fetched them.
	djenConnectorVersion = "v0"

	// djenBaseURL is the public Comunica/PJe endpoint. It takes no auth; the
	// discovery filters travel entirely in the querystring. Tests point this at
	// an httptest server via WithBaseURL.
	djenBaseURL = "https://comunicaapi.pje.jus.br/api/v1/comunicacao"

	// djenTimeout bounds each page request. A slow upstream must not pin a worker
	// goroutine; on expiry the call surfaces as SERVICE_UNAVAILABLE and the sync
	// use case re-runs the window later.
	djenTimeout = 30 * time.Second

	// djenUserAgent identifies this backend to the provider instead of leaking
	// Go's default client string.
	djenUserAgent = "jus-assessoria-backend/0 (+djen)"

	// itensPorPagina is the page size we request. It is the loop's stop signal:
	// a page returning fewer than this is the last one.
	itensPorPagina = 1000

	// djenWindowCap is the API's hard result ceiling per query. Collecting at or
	// above it means the window almost certainly truncated results the parser
	// will never see; we log a warning so the backlog task (window subdivision)
	// can pick it up. Splitting the window is out of scope here.
	djenWindowCap = 10000
)

// DJENConnector is the production Connector for DISCOVER_BY_OAB: it pages the
// Comunica API per OAB and folds every response into the slice's typed errors.
type DJENConnector struct {
	httpClient *http.Client
	baseURL    string
	userAgent  string
}

var _ Connector = (*DJENConnector)(nil)

// Option configures a DJENConnector (functional options — the API grows one
// option at a time without breaking callers). Tests inject WithBaseURL to point
// at an httptest server and WithHTTPClient to force a short timeout.
type Option func(*DJENConnector)

// WithBaseURL overrides the Comunica endpoint (used by tests against httptest).
func WithBaseURL(baseURL string) Option {
	return func(c *DJENConnector) { c.baseURL = baseURL }
}

// WithHTTPClient overrides the underlying *http.Client (timeouts, transport).
func WithHTTPClient(hc *http.Client) Option {
	return func(c *DJENConnector) { c.httpClient = hc }
}

// WithUserAgent overrides the User-Agent sent upstream.
func WithUserAgent(ua string) Option {
	return func(c *DJENConnector) { c.userAgent = ua }
}

// NewDJENConnector builds the connector with sensible defaults (a 30s-timeout
// http.Client, the public endpoint and a self-identifying User-Agent), then
// applies any overrides.
func NewDJENConnector(opts ...Option) *DJENConnector {
	c := &DJENConnector{
		httpClient: &http.Client{Timeout: djenTimeout},
		baseURL:    djenBaseURL,
		userAgent:  djenUserAgent,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

func (c *DJENConnector) ID() string      { return djenConnectorID }
func (c *DJENConnector) Version() string { return djenConnectorVersion }

// Capabilities advertises what this connector does: OAB discovery only. Pulling
// a single process by number is DataJud's job, not the DJEN's.
func (c *DJENConnector) Capabilities() []Capability {
	return []Capability{CapabilityDiscoverByOAB}
}

// Fetch pages every OAB in the request over the window and aggregates all raw
// items into one RawPayload, wrapped in the {scope, items} envelope the parser
// decodes. The item bytes are carried verbatim (json.RawMessage) so the parser
// sub-slice — not this connector — owns the DJEN item schema; the scope carries
// the queried OABs so the parser can flag matched lawyers. A failure on any page
// aborts the whole fetch: a partial payload would parse into a partial window and
// silently drop intimations.
func (c *DJENConnector) Fetch(ctx context.Context, req FetchRequest) (RawPayload, error) {
	scope := djenScope{OABs: make([]djenOAB, 0, len(req.OABs))}
	items := []json.RawMessage{}
	for _, oab := range req.OABs {
		scope.OABs = append(scope.OABs, djenOAB{Number: oab.Number, UF: oab.UF})

		collected, err := c.fetchOAB(ctx, oab, req.WindowFrom, req.WindowTo)
		if err != nil {
			return RawPayload{}, err
		}
		items = append(items, collected...)
	}

	body, err := json.Marshal(djenAggregate{Scope: scope, Items: items})
	if err != nil {
		return RawPayload{}, apperr.NewUnavailable("marshal djen payload", err)
	}

	return RawPayload{
		ConnectorID:      djenConnectorID,
		ConnectorVersion: djenConnectorVersion,
		Source:           SourceDJEN,
		Body:             body,
	}, nil
}

// djenAggregate is the envelope the connector marshals: the queried scope plus
// every raw comunicação, verbatim. It shares djenScope/djenOAB with the parser
// (one wire contract for the scope, no drift) but keeps Items as raw bytes — the
// connector aggregates bytes and leaves the item field schema to the parser. Its
// JSON is byte-for-byte what the parser's djenEnvelope decodes.
type djenAggregate struct {
	Scope djenScope         `json:"scope"`
	Items []json.RawMessage `json:"items"`
}

// fetchOAB pages one OAB from page 1 until a short page (fewer than itensPorPagina
// items) marks the end, returning the raw items of every page. Reaching the API's
// result ceiling within a window is logged, not split — subdivision is backlog.
func (c *DJENConnector) fetchOAB(ctx context.Context, oab OABEntry, from, to string) ([]json.RawMessage, error) {
	collected := []json.RawMessage{}
	for page := 1; ; page++ {
		params := url.Values{}
		params.Set("numeroOab", oab.Number)
		params.Set("ufOab", oab.UF)
		params.Set("dataDisponibilizacaoInicio", from)
		params.Set("dataDisponibilizacaoFim", to)
		params.Set("pagina", strconv.Itoa(page))
		params.Set("itensPorPagina", strconv.Itoa(itensPorPagina))

		var resp djenResponse
		if err := c.get(ctx, params, &resp); err != nil {
			return nil, err
		}

		collected = append(collected, resp.Items...)
		if len(resp.Items) < itensPorPagina {
			break
		}
	}

	if len(collected) >= djenWindowCap {
		slog.Warn(
			"possible djen window overflow",
			"oab", oab.Number,
			"uf", oab.UF,
			"window_from", from,
			"window_to", to,
		)
	}
	return collected, nil
}

// get performs one page GET and folds every outcome into the slice's typed
// errors: 200 → decode into out; a timeout, network failure or any non-200
// status → SERVICE_UNAVAILABLE (the fetch is retryable infra, not a bug). The
// provider's raw status/body never escapes — only the mapped Kind does.
func (c *DJENConnector) get(ctx context.Context, params url.Values, out any) error {
	endpoint := c.baseURL + "?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		// A bad URL is on us, not the provider — but from the caller's view the
		// fetch simply could not be performed, so Unavailable is the honest answer.
		return apperr.NewUnavailable("build djen request", err)
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return apperr.NewUnavailable("djen provider unreachable", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return apperr.NewUnavailable(
			fmt.Sprintf("djen provider returned status %d", resp.StatusCode),
			nil,
		)
	}

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return apperr.NewUnavailable("decode djen response", err)
	}
	return nil
}

// djenResponse is the envelope the Comunica API returns. Items stays raw
// (json.RawMessage) — the connector aggregates bytes and leaves the DJEN field
// schema to the parser; count/status/message are read for completeness only.
type djenResponse struct {
	Status  string            `json:"status"`
	Message string            `json:"message"`
	Count   int               `json:"count"`
	Items   []json.RawMessage `json:"items"`
}

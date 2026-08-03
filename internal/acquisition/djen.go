package acquisition

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/jusassessoria/platform/lib/apperr"
)

// djen.go is the REAL DJEN connector: it discovers a tenant's communications on
// the CNJ Comunica API (Diário de Justiça Eletrônico Nacional) by OAB over a
// window. The consulta endpoint is national (one OAB captures every tribunal) and
// public — no auth (authentication guards tribunals SENDING, not reading). It only
// FETCHES here; djen_parser.go turns the raw JSON into the domain shape.

const (
	// djenConnectorID/Version identify this connector in the sync_run audit row and
	// tag the RawPayload so the parser can refuse a payload it does not understand.
	djenConnectorID      = "djen"
	djenConnectorVersion = "v1"

	// djenDefaultBaseURL is the Comunica API production root. Homologation lives at
	// https://hcomunicaapi.cnj.jus.br/api/v1; override via WithDJENBaseURL.
	djenDefaultBaseURL = "https://comunicaapi.pje.jus.br/api/v1"

	// djenDefaultPageSize is how many communications to pull per page. The API caps
	// its reported count at 10000 and paginates; a page shorter than this ends the walk.
	djenDefaultPageSize = 100

	// djenUserAgent is a browser-like UA: the Comunica API sits behind a WAF that
	// intermittently 403s bot-looking clients. A transient 403 is a retryable fetch
	// fault (the sync cycle records FAILED and the scheduler re-syncs later), but a
	// realistic UA keeps the block rate low.
	djenUserAgent = "Mozilla/5.0 (compatible; jusassessoria-acquisition)"

	// djenMaxPages bounds the pagination walk defensively (200 pages × default size
	// spans the API's 10000-count cap twice over) so a misbehaving endpoint can never
	// loop forever.
	djenMaxPages = 200

	// djenStatusSuccess is the envelope status of a fulfilled consulta; anything else
	// (e.g. a validation error) carries a human message and is treated as a fault.
	djenStatusSuccess = "success"
)

// DJENConnector fetches DJEN communications over HTTP. It is safe for concurrent
// use (the http.Client is). Configure it with the functional options; the zero
// value is not usable — always build it through NewDJENConnector.
type DJENConnector struct {
	baseURL    string
	httpClient *http.Client
	pageSize   int
}

// DJENOption tunes a DJENConnector at construction.
type DJENOption func(*DJENConnector)

// WithDJENBaseURL overrides the Comunica API root (e.g. to point at homologation
// or an httptest server). An empty string keeps the default.
func WithDJENBaseURL(baseURL string) DJENOption {
	return func(c *DJENConnector) {
		if baseURL != "" {
			c.baseURL = baseURL
		}
	}
}

// WithDJENHTTPClient injects the HTTP client (timeout, transport). A nil client
// keeps the default (30s timeout).
func WithDJENHTTPClient(hc *http.Client) DJENOption {
	return func(c *DJENConnector) {
		if hc != nil {
			c.httpClient = hc
		}
	}
}

// WithDJENPageSize overrides the page size of the pagination walk. A non-positive
// value keeps the default.
func WithDJENPageSize(n int) DJENOption {
	return func(c *DJENConnector) {
		if n > 0 {
			c.pageSize = n
		}
	}
}

// NewDJENConnector builds the connector with production defaults, then applies the
// options.
func NewDJENConnector(opts ...DJENOption) *DJENConnector {
	c := &DJENConnector{
		baseURL:    djenDefaultBaseURL,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		pageSize:   djenDefaultPageSize,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

var _ Connector = (*DJENConnector)(nil)

func (c *DJENConnector) ID() string                 { return djenConnectorID }
func (c *DJENConnector) Version() string            { return djenConnectorVersion }
func (c *DJENConnector) Capabilities() []Capability { return []Capability{CapabilityDiscoverByOAB} }

// djenPayload is what the connector EMITS as RawPayload.Body: the OAB scope it
// queried (so the parser can flag which recipients matched the tenant) plus the
// deduped raw communication items. Keeping items as raw JSON keeps the item shape
// the parser's concern, not the connector's.
type djenPayload struct {
	OABs  []OABEntry        `json:"oabs"`
	Items []json.RawMessage `json:"items"`
}

// djenResponse is one page of the Comunica API consulta envelope. Items stay raw:
// the connector only needs each item's hash to dedup across OABs/pages.
type djenResponse struct {
	Status  string            `json:"status"`
	Message string            `json:"message"`
	Count   int               `json:"count"`
	Items   []json.RawMessage `json:"items"`
}

// djenItemKey peeks the one field the connector needs from an item to dedup it.
type djenItemKey struct {
	Hash string `json:"hash"`
}

// Fetch runs OAB discovery: it walks the consulta endpoint for each OAB in the
// request over the window, dedups communications by hash across OABs and pages
// (the same process reaches both of its advogados), and returns them as one
// djenPayload. A transport or non-success response is a retryable infra error —
// the sync use case records a FAILED run and acks so the scheduler re-syncs.
func (c *DJENConnector) Fetch(ctx context.Context, req FetchRequest) (RawPayload, error) {
	if req.Capability != CapabilityDiscoverByOAB {
		return RawPayload{}, fmt.Errorf("djen: unsupported capability %q (only %s)", req.Capability, CapabilityDiscoverByOAB)
	}

	seen := make(map[string]bool)
	items := make([]json.RawMessage, 0)
	for _, oab := range req.OABs {
		if err := c.fetchOAB(ctx, oab, req.WindowFrom, req.WindowTo, seen, &items); err != nil {
			return RawPayload{}, err
		}
	}

	body, err := json.Marshal(djenPayload{OABs: req.OABs, Items: items})
	if err != nil {
		return RawPayload{}, apperr.NewInfra("djen: marshal payload", err)
	}
	return RawPayload{
		ConnectorID:      djenConnectorID,
		ConnectorVersion: djenConnectorVersion,
		Source:           SourceDJEN,
		Body:             body,
	}, nil
}

// fetchOAB paginates one OAB's window, appending each not-yet-seen item to out.
// The walk ends when a page returns fewer items than the page size (the last
// page) or the defensive page cap is hit.
func (c *DJENConnector) fetchOAB(ctx context.Context, oab OABEntry, from, to string, seen map[string]bool, out *[]json.RawMessage) error {
	for page := 1; page <= djenMaxPages; page++ {
		resp, err := c.getPage(ctx, oab, from, to, page)
		if err != nil {
			return err
		}
		for _, raw := range resp.Items {
			var key djenItemKey
			if err := json.Unmarshal(raw, &key); err != nil {
				return apperr.NewInfra("djen: decode item hash", err)
			}
			if key.Hash != "" {
				if seen[key.Hash] {
					continue
				}
				seen[key.Hash] = true
			}
			*out = append(*out, raw)
		}
		if len(resp.Items) < c.pageSize {
			return nil
		}
	}
	return nil
}

// getPage performs one consulta GET and returns the decoded envelope, failing on a
// transport error, a non-200 status, or a non-success envelope.
func (c *DJENConnector) getPage(ctx context.Context, oab OABEntry, from, to string, page int) (djenResponse, error) {
	endpoint := c.baseURL + "/comunicacao?" + url.Values{
		"numeroOab":                  {oab.Number},
		"ufOab":                      {oab.UF},
		"dataDisponibilizacaoInicio": {from},
		"dataDisponibilizacaoFim":    {to},
		"pagina":                     {strconv.Itoa(page)},
		"itensPorPagina":             {strconv.Itoa(c.pageSize)},
	}.Encode()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return djenResponse{}, apperr.NewInfra("djen: build request", err)
	}
	httpReq.Header.Set("User-Agent", djenUserAgent)
	httpReq.Header.Set("Accept", "application/json")

	res, err := c.httpClient.Do(httpReq)
	if err != nil {
		return djenResponse{}, apperr.NewInfra(fmt.Sprintf("djen: GET comunicacao (oab %s/%s page %d)", oab.Number, oab.UF, page), err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return djenResponse{}, apperr.NewInfra(
			fmt.Sprintf("djen: comunicacao returned HTTP %d (oab %s/%s page %d)", res.StatusCode, oab.Number, oab.UF, page),
			fmt.Errorf("unexpected status %d", res.StatusCode),
		)
	}

	var envelope djenResponse
	if err := json.NewDecoder(res.Body).Decode(&envelope); err != nil {
		return djenResponse{}, apperr.NewInfra("djen: decode response", err)
	}
	if envelope.Status != djenStatusSuccess {
		return djenResponse{}, apperr.NewInfra(
			fmt.Sprintf("djen: consulta rejected (oab %s/%s): %s", oab.Number, oab.UF, envelope.Message),
			fmt.Errorf("envelope status %q", envelope.Status),
		)
	}
	return envelope, nil
}

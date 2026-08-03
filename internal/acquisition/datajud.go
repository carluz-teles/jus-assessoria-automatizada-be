package acquisition

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"github.com/jusassessoria/platform/lib/apperr"
)

// datajud.go is the REAL DATAJUD connector: it ENRICHES a known process on the
// CNJ DATAJUD Public API (an ElasticSearch index per tribunal). Unlike DJEN it
// never discovers — it needs a numeroProcesso and the tribunal whose index holds
// it — so it advertises only FETCH_BY_NUMBER. The read API is public but keyed
// (rate limiting, not auth); the key CNJ publishes is the default. datajud_parser.go
// turns the raw ES hit into the graded court record + its movimentos.

const (
	// datajudConnectorID/Version identify this connector in the sync_run audit row
	// and tag the RawPayload so the parser can refuse a payload it does not understand.
	datajudConnectorID      = "datajud"
	datajudConnectorVersion = "v1"

	// datajudDefaultBaseURL is the DATAJUD Public API root; per-tribunal indices hang
	// off it as /api_publica_<sigla>/_search.
	datajudDefaultBaseURL = "https://api-publica.datajud.cnj.jus.br"

	// datajudPublicAPIKey is the read key CNJ PUBLISHES for the public API (it gates
	// rate, not access — it is not a secret). Override via WithDATAJUDAPIKey when the
	// deployment carries its own in an env var.
	datajudPublicAPIKey = "cDZHYzlZa0JadVREZDJCendQbXY6SkJlTzNjLV9TRENyQk1RdnFKZGRQdw=="

	// datajudDefaultRatePerMinute is the public API's documented ceiling (~120 req/min).
	datajudDefaultRatePerMinute = 120

	// datajudAliasPrefix is the per-tribunal index alias prefix: "api_publica_" + the
	// lowercased tribunal sigla (TJRS → api_publica_tjrs).
	datajudAliasPrefix = "api_publica_"
)

// DATAJUDConnector fetches one process from the DATAJUD Public API. It is safe for
// concurrent use; the rate limiter serializes requests to the API's ceiling.
type DATAJUDConnector struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
	limiter    *rate.Limiter
}

// DATAJUDOption tunes a DATAJUDConnector at construction.
type DATAJUDOption func(*DATAJUDConnector)

// WithDATAJUDBaseURL overrides the API root (e.g. an httptest server). Empty keeps
// the default.
func WithDATAJUDBaseURL(baseURL string) DATAJUDOption {
	return func(c *DATAJUDConnector) {
		if baseURL != "" {
			c.baseURL = baseURL
		}
	}
}

// WithDATAJUDAPIKey overrides the published public key with a deployment's own.
// Empty keeps the default.
func WithDATAJUDAPIKey(key string) DATAJUDOption {
	return func(c *DATAJUDConnector) {
		if key != "" {
			c.apiKey = key
		}
	}
}

// WithDATAJUDHTTPClient injects the HTTP client. Nil keeps the default (30s timeout).
func WithDATAJUDHTTPClient(hc *http.Client) DATAJUDOption {
	return func(c *DATAJUDConnector) {
		if hc != nil {
			c.httpClient = hc
		}
	}
}

// WithDATAJUDRatePerMinute overrides the request ceiling. A non-positive value keeps
// the default.
func WithDATAJUDRatePerMinute(n int) DATAJUDOption {
	return func(c *DATAJUDConnector) {
		if n > 0 {
			c.limiter = rate.NewLimiter(rate.Every(time.Minute/time.Duration(n)), 1)
		}
	}
}

// NewDATAJUDConnector builds the connector with public defaults, then applies the
// options.
func NewDATAJUDConnector(opts ...DATAJUDOption) *DATAJUDConnector {
	c := &DATAJUDConnector{
		baseURL:    datajudDefaultBaseURL,
		apiKey:     datajudPublicAPIKey,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		limiter:    rate.NewLimiter(rate.Every(time.Minute/time.Duration(datajudDefaultRatePerMinute)), 1),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

var _ Connector = (*DATAJUDConnector)(nil)

func (c *DATAJUDConnector) ID() string                 { return datajudConnectorID }
func (c *DATAJUDConnector) Version() string            { return datajudConnectorVersion }
func (c *DATAJUDConnector) Capabilities() []Capability { return []Capability{CapabilityFetchByNumber} }

// datajudSearch is the ElasticSearch query body: an exact match on numeroProcesso.
type datajudSearch struct {
	Query datajudMatchQuery `json:"query"`
}

type datajudMatchQuery struct {
	Match datajudMatch `json:"match"`
}

type datajudMatch struct {
	NumeroProcesso string `json:"numeroProcesso"`
}

// Fetch pulls one process from the tribunal's index by number. A missing number or
// court is a programming error (the caller resolved neither), surfaced as a plain
// error; a transport or non-200 response is a retryable infra error (the enrichment
// use case lets asynq re-deliver — a DATAJUD rate-limit is transient). The raw ES
// envelope becomes the RawPayload the parser reads.
func (c *DATAJUDConnector) Fetch(ctx context.Context, req FetchRequest) (RawPayload, error) {
	if req.Capability != CapabilityFetchByNumber {
		return RawPayload{}, fmt.Errorf("datajud: unsupported capability %q (only %s)", req.Capability, CapabilityFetchByNumber)
	}
	if req.CNJNumber == "" || req.Court == "" {
		return RawPayload{}, fmt.Errorf("datajud: fetch by number needs both cnj number and court, got %q/%q", req.CNJNumber, req.Court)
	}

	if err := c.limiter.Wait(ctx); err != nil {
		// ctx cancelled/expired while throttled — a transient infra condition.
		return RawPayload{}, apperr.NewInfra("datajud: rate limiter wait", err)
	}

	body, err := json.Marshal(datajudSearch{Query: datajudMatchQuery{Match: datajudMatch{NumeroProcesso: req.CNJNumber}}})
	if err != nil {
		return RawPayload{}, apperr.NewInfra("datajud: marshal query", err)
	}

	endpoint := c.baseURL + "/" + datajudAliasPrefix + strings.ToLower(strings.TrimSpace(req.Court)) + "/_search"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return RawPayload{}, apperr.NewInfra("datajud: build request", err)
	}
	httpReq.Header.Set("Authorization", "APIKey "+c.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	res, err := c.httpClient.Do(httpReq)
	if err != nil {
		return RawPayload{}, apperr.NewInfra(fmt.Sprintf("datajud: POST %s (%s)", endpoint, req.CNJNumber), err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return RawPayload{}, apperr.NewInfra(
			fmt.Sprintf("datajud: search returned HTTP %d (%s %s)", res.StatusCode, req.Court, req.CNJNumber),
			fmt.Errorf("unexpected status %d", res.StatusCode),
		)
	}

	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return RawPayload{}, apperr.NewInfra("datajud: read response", err)
	}
	return RawPayload{
		ConnectorID:      datajudConnectorID,
		ConnectorVersion: datajudConnectorVersion,
		Source:           SourceDATAJUD,
		Body:             raw,
	}, nil
}

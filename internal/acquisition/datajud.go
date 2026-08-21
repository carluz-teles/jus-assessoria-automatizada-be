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

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/time/rate"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/obs"
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

func (c *DATAJUDConnector) ID() string      { return datajudConnectorID }
func (c *DATAJUDConnector) Version() string { return datajudConnectorVersion }
func (c *DATAJUDConnector) Capabilities() []Capability {
	return []Capability{CapabilityFetchByNumber, CapabilityFetchBatch}
}

// datajudBatchPageSize is the ES `terms` page cap: DATAJUD caps a _search page at
// 1000 hits, so a `terms` list longer than this — or a page that comes back FULL —
// is paged with search_after. It is the size sent per request; one limiter.Wait per
// request keeps the batch under the API's request-per-minute ceiling.
const datajudBatchPageSize = 1000

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

// datajudTermsSearch is the batch query body: a `terms` filter on the PLAIN
// numeroProcesso field (not .keyword — confirmed live) returning up to Size hits,
// sorted by numeroProcesso so SearchAfter can page a result that exceeds the cap.
type datajudTermsSearch struct {
	Size        int                `json:"size"`
	Sort        []datajudSortField `json:"sort"`
	Query       datajudTermsQuery  `json:"query"`
	SearchAfter []any              `json:"search_after,omitempty"`
}

type datajudSortField struct {
	NumeroProcesso string `json:"numeroProcesso.keyword"`
}

type datajudTermsQuery struct {
	Terms datajudTerms `json:"terms"`
}

type datajudTerms struct {
	NumeroProcesso []string `json:"numeroProcesso"`
}

// Fetch pulls one process from the tribunal's index by number. A missing number or
// court is a programming error (the caller resolved neither), surfaced as a plain
// error; a transport or non-200 response is a retryable infra error (the enrichment
// use case lets asynq re-deliver — a DATAJUD rate-limit is transient). The raw ES
// envelope becomes the RawPayload the parser reads.
func (c *DATAJUDConnector) Fetch(ctx context.Context, req FetchRequest) (_ RawPayload, err error) {
	ctx, span := obs.Start(ctx, "datajud.fetch_by_number", trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("datajud.court", req.Court),
			attribute.String("datajud.cnj_number", req.CNJNumber),
		))
	defer func() { obs.Record(span, err); span.End() }()

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
	span.SetAttributes(attribute.Int("http.response.status_code", res.StatusCode))

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

// FetchBatch pulls MANY processes of ONE tribunal in as few requests as possible: an
// ES `terms` query on the PLAIN numeroProcesso field, size ≤ datajudBatchPageSize.
// It costs ONE limiter.Wait (one request-per-minute token) per request, so a whole
// tribunal's backlog (thousands of processes) drains in a handful of requests instead
// of one-per-process. It pages with search_after when the list exceeds the cap OR a
// page returns FULL (== size — more may remain). It returns one RawPayload per page;
// each hit is matched back to its numeroProcesso by the caller's parser (a requested
// number with no hit is simply absent). A transport or non-200 is retryable infra.
func (c *DATAJUDConnector) FetchBatch(ctx context.Context, req FetchRequest) (_ []RawPayload, err error) {
	ctx, span := obs.Start(ctx, "datajud.fetch_batch", trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("datajud.court", req.Court),
			attribute.Int("datajud.batch_size", len(req.CNJNumbers)),
		))
	defer func() { obs.Record(span, err); span.End() }()

	if req.Capability != CapabilityFetchBatch {
		return nil, fmt.Errorf("datajud: unsupported capability %q (only %s)", req.Capability, CapabilityFetchBatch)
	}
	if req.Court == "" || len(req.CNJNumbers) == 0 {
		return nil, fmt.Errorf("datajud: fetch batch needs a court and at least one number, got %q/%d", req.Court, len(req.CNJNumbers))
	}

	endpoint := c.baseURL + "/" + datajudAliasPrefix + strings.ToLower(strings.TrimSpace(req.Court)) + "/_search"
	size := datajudBatchPageSize
	if len(req.CNJNumbers) < size {
		size = len(req.CNJNumbers)
	}

	pages := make([]RawPayload, 0, 1)
	var searchAfter []any
	for {
		raw, sort, hits, ferr := c.fetchTermsPage(ctx, endpoint, req.Court, req.CNJNumbers, size, searchAfter)
		if ferr != nil {
			return nil, ferr
		}
		pages = append(pages, raw)

		// A short page means the index is exhausted for these numbers. A FULL page
		// (hits == size) MAY have more, so page on with the last hit's sort token. No
		// token (unexpected) stops the loop to avoid an infinite fetch.
		if hits < size || len(sort) == 0 {
			break
		}
		searchAfter = sort
	}
	return pages, nil
}

// fetchTermsPage issues ONE `terms` _search page and returns the raw payload, the last
// hit's sort token (for search_after), and the page's hit count. One limiter.Wait per
// call is the request-per-minute budget.
func (c *DATAJUDConnector) fetchTermsPage(ctx context.Context, endpoint, court string, numbers []string, size int, searchAfter []any) (RawPayload, []any, int, error) {
	if err := c.limiter.Wait(ctx); err != nil {
		return RawPayload{}, nil, 0, apperr.NewInfra("datajud: rate limiter wait", err)
	}

	body, err := json.Marshal(datajudTermsSearch{
		Size:        size,
		Sort:        []datajudSortField{{NumeroProcesso: "asc"}},
		Query:       datajudTermsQuery{Terms: datajudTerms{NumeroProcesso: numbers}},
		SearchAfter: searchAfter,
	})
	if err != nil {
		return RawPayload{}, nil, 0, apperr.NewInfra("datajud: marshal terms query", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return RawPayload{}, nil, 0, apperr.NewInfra("datajud: build batch request", err)
	}
	httpReq.Header.Set("Authorization", "APIKey "+c.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	res, err := c.httpClient.Do(httpReq)
	if err != nil {
		return RawPayload{}, nil, 0, apperr.NewInfra(fmt.Sprintf("datajud: POST %s (batch %s)", endpoint, court), err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return RawPayload{}, nil, 0, apperr.NewInfra(
			fmt.Sprintf("datajud: batch search returned HTTP %d (%s)", res.StatusCode, court),
			fmt.Errorf("unexpected status %d", res.StatusCode),
		)
	}

	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return RawPayload{}, nil, 0, apperr.NewInfra("datajud: read batch response", err)
	}

	sort, hits, err := datajudPageCursor(raw)
	if err != nil {
		return RawPayload{}, nil, 0, apperr.NewInfra("datajud: decode batch cursor", err)
	}
	return RawPayload{
		ConnectorID:      datajudConnectorID,
		ConnectorVersion: datajudConnectorVersion,
		Source:           SourceDATAJUD,
		Body:             raw,
	}, sort, hits, nil
}

// datajudPageCursor reads a batch page's hit count and the LAST hit's sort token off
// the ES envelope, so the connector pages with search_after without depending on the
// parser. It reads only the pagination shell (hits[].sort), never the _source the
// parser owns.
func datajudPageCursor(body []byte) (lastSort []any, hitCount int, err error) {
	var env struct {
		Hits struct {
			Hits []struct {
				Sort []any `json:"sort"`
			} `json:"hits"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, 0, err
	}
	hitCount = len(env.Hits.Hits)
	if hitCount == 0 {
		return nil, 0, nil
	}
	return env.Hits.Hits[hitCount-1].Sort, hitCount, nil
}

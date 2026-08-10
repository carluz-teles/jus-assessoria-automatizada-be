package acquisition

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"golang.org/x/time/rate"

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

	// The Comunica API sits behind a WAF that 403s bot-looking clients — from a
	// datacenter IP (prod) consistently, and it also rate-blocks bursts. So the
	// connector presents a FULL, current browser fingerprint (real Chrome UA +
	// Referer to the public consulta site + Accept-Language) and paces itself
	// (djenDefaultRatePerMinute). A 403 that still slips through is a retryable
	// fetch fault — asynq re-delivers with backoff.
	djenUserAgent      = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"
	djenReferer        = "https://comunica.pje.jus.br/"
	djenAcceptLanguage = "pt-BR,pt;q=0.9,en;q=0.8"

	// djenDefaultRatePerMinute paces requests so the backfill's burst of windows does
	// not trip the WAF's rate block. The limiter is shared across the connector, so it
	// caps the total request rate even under concurrent syncs. Override per env
	// (DJEN_RATE_PER_MINUTE) via WithDJENRatePerMinute when a big OAB sweep needs a
	// gentler pace than 1 req/s.
	djenDefaultRatePerMinute = 60

	// Cooldown (circuit-breaker) after a 429/503: the shared gate makes ALL concurrent
	// slices pause together, so a rate-limited egress IP recovers instead of being
	// hammered by 53 windows × their retries. The pause grows exponentially per
	// consecutive rate-limit (djenCooldownBase << n), capped at djenCooldownMax, and
	// resets on the first successful page. A server Retry-After (when present) wins,
	// still capped so a worker slot is never held too long.
	djenCooldownBase = 2 * time.Second
	djenCooldownMax  = 30 * time.Second

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
	limiter    *rate.Limiter
	cooldown   *cooldownGate
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

// WithDJENRatePerMinute overrides the request pace (WAF avoidance). A non-positive
// value keeps the default.
func WithDJENRatePerMinute(n int) DJENOption {
	return func(c *DJENConnector) {
		if n > 0 {
			c.limiter = rate.NewLimiter(rate.Every(time.Minute/time.Duration(n)), 1)
		}
	}
}

// WithDJENProxy routes outbound requests through an HTTP/SOCKS proxy — the
// production fix for the Comunica WAF, which 403s the Railway datacenter egress IP
// even with browser headers + pacing (the block is IP/geo-based). A residential/BR
// proxy presents a clean egress IP. It sets the Transport on the existing client,
// preserving its timeout; a nil URL keeps the direct connection (dev local passes
// without it).
func WithDJENProxy(proxyURL *url.URL) DJENOption {
	return func(c *DJENConnector) {
		if proxyURL == nil {
			return
		}
		c.httpClient.Transport = &http.Transport{Proxy: http.ProxyURL(proxyURL)}
	}
}

// NewDJENConnector builds the connector with production defaults, then applies the
// options.
func NewDJENConnector(opts ...DJENOption) *DJENConnector {
	c := &DJENConnector{
		baseURL:    djenDefaultBaseURL,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		pageSize:   djenDefaultPageSize,
		limiter:    rate.NewLimiter(rate.Every(time.Minute/time.Duration(djenDefaultRatePerMinute)), 1),
		cooldown:   newCooldownGate(),
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

	// Back off together with every other slice while a rate block is cooling down,
	// then pace this request through the shared limiter.
	if err := c.cooldown.wait(ctx); err != nil {
		return djenResponse{}, apperr.NewInfra("djen: cooldown wait", err)
	}
	if err := c.limiter.Wait(ctx); err != nil {
		return djenResponse{}, apperr.NewInfra("djen: rate limiter wait", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return djenResponse{}, apperr.NewInfra("djen: build request", err)
	}
	// Present a full, current browser fingerprint — the WAF 403s bot-looking clients.
	httpReq.Header.Set("User-Agent", djenUserAgent)
	httpReq.Header.Set("Accept", "application/json, text/plain, */*")
	httpReq.Header.Set("Accept-Language", djenAcceptLanguage)
	httpReq.Header.Set("Referer", djenReferer)
	httpReq.Header.Set("Origin", "https://comunica.pje.jus.br")

	res, err := c.httpClient.Do(httpReq)
	if err != nil {
		return djenResponse{}, apperr.NewInfra(fmt.Sprintf("djen: GET comunicacao (oab %s/%s page %d)", oab.Number, oab.UF, page), err)
	}
	defer res.Body.Close()

	// A 429 (or 503) is a rate block, not a fault: trip the shared cooldown so every
	// slice backs off together, and surface a typed, retryable Unavailable carrying
	// the server's Retry-After.
	if res.StatusCode == http.StatusTooManyRequests || res.StatusCode == http.StatusServiceUnavailable {
		retryAfter := parseRetryAfter(res.Header.Get("Retry-After"), c.cooldown.now())
		c.cooldown.trip(retryAfter)
		return djenResponse{}, apperr.NewUnavailable(
			fmt.Sprintf("djen: comunicacao rate limited (oab %s/%s page %d)", oab.Number, oab.UF, page),
			&RateLimitedError{Status: res.StatusCode, RetryAfter: retryAfter, OAB: oab.Number, UF: oab.UF, Page: page},
		)
	}
	if res.StatusCode != http.StatusOK {
		return djenResponse{}, apperr.NewInfra(
			fmt.Sprintf("djen: comunicacao returned HTTP %d (oab %s/%s page %d)", res.StatusCode, oab.Number, oab.UF, page),
			fmt.Errorf("unexpected status %d", res.StatusCode),
		)
	}
	// A clean page means the egress IP is healthy again — clear the backoff streak.
	c.cooldown.reset()

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

// RateLimitedError signals DJEN returned 429 (or 503) — a transient rate block, not a
// bug in our code. It carries the server's suggested wait (Retry-After; 0 when absent)
// so the cooldown and any retry policy can honor it. It rides as the cause of an
// apperr.Unavailable, so errors.As can recover it while the outer error stays
// HTTP-agnostic and retryable.
type RateLimitedError struct {
	Status     int
	RetryAfter time.Duration
	OAB        string
	UF         string
	Page       int
}

func (e *RateLimitedError) Error() string {
	return fmt.Sprintf(
		"djen rate limited: HTTP %d (oab %s/%s page %d), retry after %s",
		e.Status, e.OAB, e.UF, e.Page, e.RetryAfter,
	)
}

// cooldownGate is the shared circuit-breaker for the DJEN egress. On a rate block it
// pushes forward a single deadline that EVERY concurrent slice waits on before its
// next request — so the whole connector backs off together and the rate-limited IP
// recovers, instead of 53 windows × their retries hammering it. The pause grows per
// consecutive block (exponential, capped) and resets on the first clean page. Safe
// for concurrent use.
type cooldownGate struct {
	mu          sync.Mutex
	until       time.Time
	consecutive int
	now         func() time.Time
}

func newCooldownGate() *cooldownGate {
	return &cooldownGate{now: time.Now}
}

// wait blocks until the current cooldown deadline passes or ctx is cancelled. It is a
// no-op on the common path (no active cooldown).
func (g *cooldownGate) wait(ctx context.Context) error {
	g.mu.Lock()
	d := g.until.Sub(g.now())
	g.mu.Unlock()
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// trip extends the shared cooldown after a rate block and returns the pause applied.
// A server Retry-After wins when present; otherwise the pause is exponential in the
// consecutive-block count. Either way it is capped at djenCooldownMax so a worker slot
// is never held too long. The shift is clamped so it can never overflow.
func (g *cooldownGate) trip(retryAfter time.Duration) time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()

	d := retryAfter
	if d <= 0 {
		shift := g.consecutive
		if shift > 5 {
			shift = 5
		}
		d = djenCooldownBase << shift
	}
	if d <= 0 || d > djenCooldownMax {
		d = djenCooldownMax
	}
	g.consecutive++

	if until := g.now().Add(d); until.After(g.until) {
		g.until = until
	}
	return d
}

// reset clears the consecutive-block streak after a clean page, so the next block
// starts its backoff from the base again.
func (g *cooldownGate) reset() {
	g.mu.Lock()
	g.consecutive = 0
	g.mu.Unlock()
}

// parseRetryAfter reads a Retry-After header (delta-seconds or HTTP-date) into a
// duration, using now to turn a date into a delta. Returns 0 when absent, negative,
// or unparseable — the caller then falls back to its exponential default.
func parseRetryAfter(h string, now time.Time) time.Duration {
	if h == "" {
		return 0
	}
	if secs, err := strconv.Atoi(h); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

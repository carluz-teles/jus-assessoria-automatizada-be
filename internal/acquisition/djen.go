package acquisition

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"
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

	// djenDefaultPageSize is how many communications to pull per page. The Comunica API
	// honors itensPorPagina up to 1000 (verified) and caps its reported count at 10000;
	// a page shorter than this ends the walk. At 1000 a weekly backfill window is
	// usually ONE request instead of dozens — ~10× fewer calls against the 1 req/s
	// pace, so a big-OAB sweep finishes in minutes, not hours. Override via
	// WithDJENPageSize / DJEN_PAGE_SIZE.
	djenDefaultPageSize = 1000

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
	// proxyURL, when set, is the HTTP CONNECT proxy the uTLS transport tunnels through
	// (residential/BR egress for the WAF's IP-reputation 403). The uTLS handshake runs
	// over the tunnel, so the proxy is applied inside DialTLSContext, not Transport.Proxy.
	proxyURL *url.URL
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

// WithDJENProxy routes outbound requests through an HTTP CONNECT proxy — the
// production fix for the Comunica WAF, which 403s the Railway datacenter egress IP
// even with browser headers + pacing (the block is IP/geo-based). A residential/BR
// proxy presents a clean egress IP. It only records the URL; NewDJENConnector wires it
// into the uTLS transport (the handshake runs over the proxy's CONNECT tunnel). A nil
// URL keeps the direct connection (dev local passes without it, still via uTLS).
func WithDJENProxy(proxyURL *url.URL) DJENOption {
	return func(c *DJENConnector) {
		c.proxyURL = proxyURL
	}
}

// NewDJENConnector builds the connector with production defaults, then applies the
// options.
func NewDJENConnector(opts ...DJENOption) *DJENConnector {
	c := &DJENConnector{
		baseURL: djenDefaultBaseURL,
		// 60s (not 30): a page of 1000 items is a big response, and the WAF/proxy can be
		// slow — a tight timeout turns a healthy fetch into a spurious retry.
		httpClient: &http.Client{Timeout: 60 * time.Second},
		pageSize:   djenDefaultPageSize,
		limiter:    rate.NewLimiter(rate.Every(time.Minute/time.Duration(djenDefaultRatePerMinute)), 1),
		cooldown:   newCooldownGate(),
	}
	for _, opt := range opts {
		opt(c)
	}
	// Present the Chrome TLS fingerprint. The Comunica WAF rate-limits by JA3: Go's
	// default ClientHello 429s after ~8 requests where curl/Chrome sail through (PROVEN
	// by same-IP probes). uTLS gives us a browser-sized budget. Skipped when a custom
	// client was injected (WithDJENHTTPClient, e.g. an httptest client), so tests keep
	// their transport.
	if c.httpClient.Transport == nil {
		c.httpClient.Transport = newDJENTransport(c.proxyURL)
	}
	return c
}

var _ Connector = (*DJENConnector)(nil)

// djenChromeHello is the uTLS ClientHello preset the connector presents — a Chrome
// fingerprint, so the WAF's per-JA3 rate limiter gives us a browser budget, not the
// bot-sized one Go's default handshake earns.
var djenChromeHello = utls.HelloChrome_120

// newDJENTransport builds the HTTP transport whose TLS layer presents the Chrome
// fingerprint via uTLS, optionally tunneling through an HTTP CONNECT proxy. The proxy is
// handled INSIDE DialTLSContext (not Transport.Proxy) because the uTLS handshake must run
// over the tunneled conn — the Proxy field would run the stdlib's own TLS instead.
func newDJENTransport(proxyURL *url.URL) *http.Transport {
	return &http.Transport{
		ForceAttemptHTTP2: false, // the uTLS hello pins ALPN to http/1.1
		MaxIdleConns:      10,
		IdleConnTimeout:   90 * time.Second,
		DialTLSContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			return dialDJENChromeTLS(ctx, proxyURL, addr)
		},
	}
}

// dialDJENChromeTLS opens the TCP conn (direct or via the proxy's CONNECT tunnel) and
// completes a uTLS handshake presenting the Chrome ClientHello, with ALPN pinned to
// http/1.1 so net/http speaks HTTP/1.1 over it (the cipher/extension fingerprint the WAF
// keys on is preserved).
func dialDJENChromeTLS(ctx context.Context, proxyURL *url.URL, addr string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	var raw net.Conn
	var err error
	if proxyURL != nil {
		raw, err = proxyConnect(ctx, dialer, proxyURL, addr)
	} else {
		raw, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, err
	}

	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		_ = raw.Close()
		return nil, err
	}
	spec, err := utls.UTLSIdToSpec(djenChromeHello)
	if err != nil {
		_ = raw.Close()
		return nil, err
	}
	for _, ext := range spec.Extensions {
		if a, ok := ext.(*utls.ALPNExtension); ok {
			a.AlpnProtocols = []string{"http/1.1"}
		}
	}
	u := utls.UClient(raw, &utls.Config{ServerName: host}, utls.HelloCustom)
	if err := u.ApplyPreset(&spec); err != nil {
		_ = raw.Close()
		return nil, err
	}
	if err := u.HandshakeContext(ctx); err != nil {
		_ = raw.Close()
		return nil, err
	}
	return u, nil
}

// proxyConnect dials the proxy and opens an HTTP CONNECT tunnel to target, returning the
// tunneled conn for the caller to run TLS over. Basic auth from the proxy URL userinfo is
// sent when present.
func proxyConnect(ctx context.Context, dialer *net.Dialer, proxyURL *url.URL, target string) (net.Conn, error) {
	conn, err := dialer.DialContext(ctx, "tcp", proxyHostPort(proxyURL))
	if err != nil {
		return nil, fmt.Errorf("dial proxy: %w", err)
	}
	req := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n", target, target)
	if proxyURL.User != nil {
		user := proxyURL.User.Username()
		pass, _ := proxyURL.User.Password()
		cred := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
		req += "Proxy-Authorization: Basic " + cred + "\r\n"
	}
	req += "\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("proxy CONNECT write: %w", err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("proxy CONNECT read: %w", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_ = conn.Close()
		return nil, fmt.Errorf("proxy CONNECT %s: status %s", target, resp.Status)
	}
	return conn, nil
}

// proxyHostPort returns the proxy's host:port, defaulting the port by scheme when omitted.
func proxyHostPort(u *url.URL) string {
	if u.Port() != "" {
		return u.Host
	}
	if u.Scheme == "https" {
		return u.Hostname() + ":443"
	}
	return u.Hostname() + ":80"
}

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

// FetchDiario walks one tribunal's diário over [from, to], paginating the consulta
// filtered by siglaTribunal (no OAB), and returns every raw communication item. This
// is the national-ingestion read: the caller lands the items in the publication store
// and matches OABs locally. Unlike the per-OAB Fetch it needs no cross-query dedup (a
// tribunal page's items are already unique), so a straight accumulate suffices. The
// walk ends at the first short page or the defensive page cap.
func (c *DJENConnector) FetchDiario(ctx context.Context, tribunal, from, to string) ([]json.RawMessage, error) {
	items := make([]json.RawMessage, 0)
	for page := 1; page <= djenMaxPages; page++ {
		resp, err := c.getDiarioPage(ctx, tribunal, from, to, page)
		if err != nil {
			return nil, err
		}
		items = append(items, resp.Items...)
		if len(resp.Items) < c.pageSize {
			break
		}
	}
	return items, nil
}

// getPage performs one per-OAB consulta GET (numeroOab/ufOab filter) and returns the
// decoded envelope. The HTTP/rate machinery is shared with the diário read via
// requestPage; the RateLimitedError carries the OAB identity for the backoff logs.
func (c *DJENConnector) getPage(ctx context.Context, oab OABEntry, from, to string, page int) (djenResponse, error) {
	query := url.Values{
		"numeroOab":                  {oab.Number},
		"ufOab":                      {oab.UF},
		"dataDisponibilizacaoInicio": {from},
		"dataDisponibilizacaoFim":    {to},
		"pagina":                     {strconv.Itoa(page)},
		"itensPorPagina":             {strconv.Itoa(c.pageSize)},
	}
	label := fmt.Sprintf("oab %s/%s page %d", oab.Number, oab.UF, page)
	return c.requestPage(ctx, query, label, RateLimitedError{OAB: oab.Number, UF: oab.UF, Page: page})
}

// getDiarioPage performs one by-tribunal consulta GET (siglaTribunal filter, no OAB)
// — the national ingestion read. Same shared machinery; the RateLimitedError carries
// the page (the tribunal is in the error label).
func (c *DJENConnector) getDiarioPage(ctx context.Context, tribunal, from, to string, page int) (djenResponse, error) {
	query := url.Values{
		"siglaTribunal":              {tribunal},
		"dataDisponibilizacaoInicio": {from},
		"dataDisponibilizacaoFim":    {to},
		"pagina":                     {strconv.Itoa(page)},
		"itensPorPagina":             {strconv.Itoa(c.pageSize)},
	}
	label := fmt.Sprintf("tribunal %s page %d", tribunal, page)
	return c.requestPage(ctx, query, label, RateLimitedError{Page: page})
}

// requestPage does one consulta GET for a pre-built query and decodes the envelope,
// sharing the cooldown/limiter/backoff machinery across the per-OAB and by-tribunal
// reads. It fails on a transport error, a non-200 status, or a non-success envelope;
// a 429/503 trips the shared cooldown and surfaces onBlock (with Status/RetryAfter
// filled in) as a retryable Unavailable. label identifies the request in error text.
func (c *DJENConnector) requestPage(ctx context.Context, query url.Values, label string, onBlock RateLimitedError) (djenResponse, error) {
	endpoint := c.baseURL + "/comunicacao?" + query.Encode()

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
		return djenResponse{}, apperr.NewInfra(fmt.Sprintf("djen: GET comunicacao (%s)", label), err)
	}
	defer res.Body.Close()

	// A 429 (or 503) is a rate block, not a fault: trip the shared cooldown so every
	// slice backs off together, and surface a typed, retryable Unavailable carrying
	// the server's Retry-After.
	if res.StatusCode == http.StatusTooManyRequests || res.StatusCode == http.StatusServiceUnavailable {
		retryAfter := parseRetryAfter(res.Header.Get("Retry-After"), c.cooldown.now())
		c.cooldown.trip(retryAfter)
		onBlock.Status = res.StatusCode
		onBlock.RetryAfter = retryAfter
		return djenResponse{}, apperr.NewUnavailable(
			fmt.Sprintf("djen: comunicacao rate limited (%s)", label),
			&onBlock,
		)
	}
	if res.StatusCode != http.StatusOK {
		return djenResponse{}, apperr.NewInfra(
			fmt.Sprintf("djen: comunicacao returned HTTP %d (%s)", res.StatusCode, label),
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
			fmt.Sprintf("djen: consulta rejected (%s): %s", label, envelope.Message),
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

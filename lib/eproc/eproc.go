// Package eproc is the D0 walking skeleton of the TJSP eproc court adapter: it logs
// in against the TJSP SSO (Keycloak at sso.tjsp.jus.br), keeps the resulting session
// (cookies) and reuses it across calls, re-authenticates automatically when the portal
// answers 401 or redirects to the login page, and exposes the READ flow the ERD §6
// asks for — list a process, list its events, download a document (streamed to a
// file, never buffered whole).
//
// SPIKE SCOPE (docs/erd-execucao-judicial-tjsp.md §10.2 D0, Portão A). The MACHINE
// here — session reuse, re-auth on 401/redirect, typed errors, streamed download — is
// unit-tested without network via an httptest stub. The eproc-specific WIRING (the
// exact SSO form fields, portal paths, and HTML/JSON parsing) is deliberately THIN and
// marked "ASSUMPTION (Portão B)": it is based on research (the SSO is a real Keycloak,
// plain HTTP suffices for the login) and MUST be confirmed against the real portal
// once a lawyer credential is available. Every provisional decision is isolated behind
// the small functions in wiring.go so Portão B costs little to correct.
//
// This package does NOT import lib/transport: the caller (cmd/eproc-dump) builds the
// *http.Client (with the Chrome uTLS transport) and injects it, keeping this lib pure.
package eproc

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/jusassessoria/platform/lib/apperr"
)

// Credentials is the lawyer identity used to authenticate against the eproc SSO. It is
// held in memory only and MUST NEVER be logged — not even in debug mode.
type Credentials struct {
	Username string
	Password string
}

// authMode selects which login mechanism the client uses. Certificate mode exists
// because eproc's "Certificado Digital" option is genuinely a different mechanism from
// password login (confirmed against TJSP's own manuals): the browser's native
// certificate picker appearing is the signature of the server REQUESTING a client
// certificate during the TLS handshake (mutual TLS), not an in-page challenge/response
// like Lacuna Web PKI. The private key never enters this package — the caller's
// *http.Client is already mTLS-authenticated (via transport.ChromeTransport's
// clientCert param) before any request here is sent; certLogin only confirms the
// handshake actually established a session and surfaces the (expected) TOTP MFA step.
type authMode int

const (
	authModePassword    authMode = iota // default: username/password ROPC-style POST
	authModeCertificate                 // mutual TLS already negotiated by the transport
)

// Process is the minimal court-case shape D0 dumps: enough to prove the read reached a
// real process. Parsing is provisional (see wiring.go).
type Process struct {
	CNJNumber   string  // número CNJ (nnnnnnn-dd.aaaa.j.tr.oooo)
	Class       string  // classe processual
	JudgingBody string  // órgão julgador / vara
	Parties     []Party // partes, with the CPF/CNPJ UNKNOWN this spike resolves
}

// Party is one participant of the process. RawDocument is the CPF/CNPJ field EXACTLY as
// the portal exposes it (clear, masked, or empty) — the whole point of D0's Portão B is
// to discover which, so we carry it verbatim and never normalize it away.
type Party struct {
	Name        string
	Role        string // polo ativo/passivo, advogado, etc.
	RawDocument string // CPF/CNPJ as-returned; may be "", "123.***.***-00", or full
}

// Event is one node of the eproc event tree (docket entry).
type Event struct {
	ExternalID  string
	Date        time.Time
	Description string
	Documents   []DocumentRef
}

// DocumentRef points at a downloadable document within an event.
type DocumentRef struct {
	ExternalID string
	Label      string
	MIMEType   string
}

// SessionStatus mirrors the court_connection state machine (ERD §5) that matters to a
// stateless read client: whether we currently hold a usable session.
type SessionStatus string

const (
	// StatusDisconnected: no session yet (never logged in, or Reset called).
	StatusDisconnected SessionStatus = "DISCONNECTED"
	// StatusConnected: a login succeeded and the cookie jar holds a session.
	StatusConnected SessionStatus = "CONNECTED"
)

// Client talks to one eproc portal on behalf of one lawyer identity. It is safe for
// concurrent use: the cookie jar is guarded and re-auth is single-flighted so a burst
// of calls hitting an expired session triggers exactly one login. Build it with
// NewEprocClient; the zero value is not usable.
type Client struct {
	hc      *http.Client
	baseURL string
	creds   Credentials
	now     func() time.Time

	// mu guards status; the jar itself is concurrency-safe. authOnce single-flights
	// concurrent re-auths so N callers seeing a 401 at once cause ONE login.
	mu       sync.Mutex
	status   SessionStatus
	authOnce *authGroup

	mode authMode
}

// Option tunes a Client at construction (functional options, per the repo convention).
type Option func(*Client)

// WithBaseURL overrides the eproc portal root (default eprocDefaultBaseURL). Point it at
// an httptest server in tests. An empty string keeps the default.
func WithBaseURL(baseURL string) Option {
	return func(c *Client) {
		if baseURL != "" {
			c.baseURL = strings.TrimRight(baseURL, "/")
		}
	}
}

// WithCredentials sets the lawyer login used for authentication and automatic re-auth.
func WithCredentials(creds Credentials) Option {
	return func(c *Client) {
		c.creds = creds
	}
}

// WithCertificateAuth switches the client to certificate-login mode: Login/re-auth
// skip the username/password POST and instead confirm the session the caller's
// mTLS-configured *http.Client should already have established (see authMode's doc).
// The caller is responsible for injecting the client certificate into the transport
// (transport.ChromeTransport's clientCert param) — this package never touches key
// material.
func WithCertificateAuth() Option {
	return func(c *Client) {
		c.mode = authModeCertificate
	}
}

// WithClock overrides the time source (tests). A nil func keeps time.Now.
func WithClock(now func() time.Time) Option {
	return func(c *Client) {
		if now != nil {
			c.now = now
		}
	}
}

// NewEprocClient builds the client around an injected *http.Client. The caller owns the
// client's transport (the Chrome uTLS transport lives in lib/transport, wired by the
// CLI) — this keeps lib/eproc pure and free of the transport dependency. A nil hc falls
// back to a 60s-timeout default client so the constructor never panics, but production
// callers MUST inject the fingerprinted client.
//
// A cookie jar is installed on the client (replacing any existing jar) so the session
// established at login rides on every subsequent request automatically.
func NewEprocClient(hc *http.Client, opts ...Option) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	// Install a cookie jar so the SSO/session cookies persist across calls. cookiejar.New
	// with a nil options never returns an error in practice, but we guard defensively.
	if jar, err := cookiejar.New(nil); err == nil {
		hc.Jar = jar
	}

	c := &Client{
		hc:       hc,
		baseURL:  eprocDefaultBaseURL,
		now:      time.Now,
		status:   StatusDisconnected,
		authOnce: newAuthGroup(),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Status reports whether the client currently holds a session.
func (c *Client) Status() SessionStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

// Login authenticates against the SSO and establishes the session, storing the cookies
// in the jar. It is idempotent enough for callers to invoke explicitly; the read methods
// also trigger it lazily via ensureSession. Returns a typed error distinguishing bad
// credentials (Unauthorized), a captcha/MFA challenge (Forbidden — the spike's second
// UNKNOWN), and a network/portal fault (Unavailable).
func (c *Client) Login(ctx context.Context) error {
	if err := c.authGroupLogin(ctx); err != nil {
		return err
	}
	return nil
}

// Reset drops the session (clears the jar) and marks the client DISCONNECTED. Mainly for
// tests and for forcing a fresh login.
func (c *Client) Reset() {
	if jar, err := cookiejar.New(nil); err == nil {
		c.hc.Jar = jar
	}
	c.mu.Lock()
	c.status = StatusDisconnected
	c.mu.Unlock()
}

// GetProcess fetches one process by CNJ number, authenticating first if needed and
// re-authenticating once if the portal rejects the session mid-flight.
func (c *Client) GetProcess(ctx context.Context, cnjNumber string) (*Process, error) {
	body, err := c.doAuthed(ctx, http.MethodGet, c.processPath(cnjNumber), nil)
	if err != nil {
		return nil, err
	}
	defer body.Close()

	proc, err := parseProcess(body)
	if err != nil {
		return nil, apperr.NewUnavailable("eproc: parse process", err)
	}
	return proc, nil
}

// ListEvents fetches the event tree (docket) of a process.
func (c *Client) ListEvents(ctx context.Context, cnjNumber string) ([]Event, error) {
	body, err := c.doAuthed(ctx, http.MethodGet, c.eventsPath(cnjNumber), nil)
	if err != nil {
		return nil, err
	}
	defer body.Close()

	events, err := parseEvents(body)
	if err != nil {
		return nil, apperr.NewUnavailable("eproc: parse events", err)
	}
	return events, nil
}

// DownloadDocument streams one document to dst, returning the number of bytes written.
// It streams straight from the response body into dst (io.Copy) so a large PDF never
// materializes in memory — the D0 requirement to not OOM on downloads.
func (c *Client) DownloadDocument(ctx context.Context, docExternalID string, dst io.Writer) (int64, error) {
	body, err := c.doAuthed(ctx, http.MethodGet, c.documentPath(docExternalID), nil)
	if err != nil {
		return 0, err
	}
	defer body.Close()

	n, err := io.Copy(dst, body)
	if err != nil {
		return n, apperr.NewUnavailable("eproc: stream document", err)
	}
	return n, nil
}

// doAuthed runs an authenticated request with the re-auth-on-rejection loop. It ensures a
// session exists, sends the request, and — if the portal signals the session is stale
// (401 or a redirect to the login page) — re-authenticates ONCE and retries. A second
// rejection is surfaced as Unauthorized (the credentials no longer work) rather than
// looping. The returned ReadCloser is the (open) response body; the caller MUST close it.
func (c *Client) doAuthed(ctx context.Context, method, path string, reqBody io.Reader) (io.ReadCloser, error) {
	if err := c.ensureSession(ctx); err != nil {
		return nil, err
	}

	resp, err := c.send(ctx, method, path, reqBody)
	if err != nil {
		return nil, err
	}

	if sessionRejected(resp) {
		resp.Body.Close()
		// Session went stale between ensureSession and now (or the portal expired it):
		// force a fresh login, then retry exactly once.
		c.markDisconnected()
		if err := c.authGroupLogin(ctx); err != nil {
			return nil, err
		}
		resp, err = c.send(ctx, method, path, reqBody)
		if err != nil {
			return nil, err
		}
		if sessionRejected(resp) {
			resp.Body.Close()
			return nil, apperr.NewUnauthorized("eproc: session rejected after re-auth")
		}
	}

	if err := classifyStatus(resp); err != nil {
		resp.Body.Close()
		return nil, err
	}
	return resp.Body, nil
}

// send builds and performs one request against the portal, joining path to the base URL.
// It maps any transport error to a typed Unavailable.
func (c *Client) send(ctx context.Context, method, path string, reqBody io.Reader) (*http.Response, error) {
	endpoint := c.baseURL + path
	httpReq, err := http.NewRequestWithContext(ctx, method, endpoint, reqBody)
	if err != nil {
		return nil, apperr.NewInfra("eproc: build request", err)
	}
	applyBrowserHeaders(httpReq)

	resp, err := c.hc.Do(httpReq)
	if err != nil {
		return nil, apperr.NewUnavailable("eproc: request failed", err)
	}
	return resp, nil
}

// ensureSession logs in lazily if the client is not yet CONNECTED.
func (c *Client) ensureSession(ctx context.Context) error {
	c.mu.Lock()
	connected := c.status == StatusConnected
	c.mu.Unlock()
	if connected {
		return nil
	}
	return c.authGroupLogin(ctx)
}

func (c *Client) markConnected() {
	c.mu.Lock()
	c.status = StatusConnected
	c.mu.Unlock()
}

func (c *Client) markDisconnected() {
	c.mu.Lock()
	c.status = StatusDisconnected
	c.mu.Unlock()
}

// processPath/eventsPath/documentPath build the portal paths. They are provisional
// (Portão B) — kept as one-liners here so the real paths are a one-line fix.
func (c *Client) processPath(cnjNumber string) string {
	return eprocProcessPath(url.QueryEscape(cnjNumber))
}

func (c *Client) eventsPath(cnjNumber string) string {
	return eprocEventsPath(url.QueryEscape(cnjNumber))
}

func (c *Client) documentPath(docExternalID string) string {
	return eprocDocumentPath(url.QueryEscape(docExternalID))
}

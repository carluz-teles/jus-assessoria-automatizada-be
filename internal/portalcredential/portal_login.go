package portalcredential

import (
	"context"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// portal_login.go implements the synchronous login test against the REAL TJSP
// eproc portal (docs/erd-tribunal-scraping.md §5/§7, PortalSession/§11's first
// UNKNOWN). The URLs, form field names and error markers below were captured
// LIVE against https://eproc1g.tjsp.jus.br/eproc/ in this session (curl, both a
// clean GET and a POST with a deliberately wrong password) — they are not
// guessed. See KNOWN_LIMITATIONS in the handoff for exactly what was and was
// not exercised (a genuinely valid login was never attempted — no real
// credential was available in this session).
//
// What the live probe found: eproc TJSP's login is NOT a form posted straight to
// eproc — externo_controlador.php?acao=SSO/login redirects to a Keycloak SSO
// instance (sso.tjsp.jus.br/realms/eproc), which renders an HTML form
// (#kc-form-login, fields "username"/"password") whose POST target embeds a
// short-lived session_code/execution/tab_id/client_data — regenerated on every
// GET, so it MUST be scraped from the page just fetched, never hard-coded. This
// confirms §5's Option B premise (HTTP+HTML, no browser) for at least the login
// step: no JS execution was required to reach or submit the form.
const (
	// tjspEprocLoginStartURL kicks off the SSO redirect chain. The querystring is
	// static/harmless (empty process filters) — TJSP's own site links here.
	tjspEprocLoginStartURL = "https://eproc1g.tjsp.jus.br/eproc/externo_controlador.php?acao=SSO/login&num_processo_bi=&lista_processos="

	// tjspUserAgent presents a full, current desktop Chrome fingerprint — the
	// same convention djen.go already uses against a WAF-fronted tribunal API
	// (docs/erd-tribunal-scraping.md §5 reuses that groundwork). Untested here
	// whether eproc's edge blocks datacenter IPs the way DJEN's WAF did (see
	// KNOWN_LIMITATIONS) — this adapter accepts an injectable *http.Client
	// (WithHTTPClient) so a uTLS+proxy transport can be swapped in later without
	// touching the parsing/classification logic.
	tjspUserAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"

	// tjspLoginTimeout bounds each HTTP round trip. The portal is a live
	// government system outside our control — a hung connection must not hang
	// the caller's PUT request indefinitely.
	tjspLoginTimeout = 20 * time.Second
)

// kcFormActionRe extracts the Keycloak login form's POST target from the page
// fetched at tjspEprocLoginStartURL. Captured live: the form is
// `<form id="kc-form-login" ... action="https://sso.tjsp.jus.br/realms/eproc/
// login-actions/authenticate?session_code=...&execution=...&client_id=...
// &tab_id=...&client_data=..." method="post" ...>`. A narrow regexp (not a full
// HTML parser — no such dependency exists in the repo, and extracting one
// attribute of one known element does not justify adding one, "a little
// copying is better than a little dependency") is enough: it fails closed
// (empty match) if the markup changes, which Check treats as inconclusive
// (PARSING_BROKEN-shaped), never a false success.
var kcFormActionRe = regexp.MustCompile(`id="kc-form-login"[^>]*action="([^"]+)"`)

// kcLoginErrorRe matches Keycloak's rejection markup, captured live from a real
// wrong-password POST: the page re-renders with
// `<span id="input-error" ... aria-live="polite">Nome de usuário ou senha
// inválida.</span>`. Both the element id and the Portuguese message are
// checked (either alone could drift independently across a Keycloak theme
// update) so a partial match still classifies correctly.
var kcLoginErrorRe = regexp.MustCompile(`(?i)id="input-error"|nome de usu[aá]rio ou senha inv[aá]lid`)

// captchaMarkerRe flags a captcha challenge if the portal ever presents one.
// Never observed live in this session (see KNOWN_LIMITATIONS) — Keycloak's
// brute-force-detection theme can add one after repeated failures, so this is
// a defensive check, not a confirmed marker.
var captchaMarkerRe = regexp.MustCompile(`(?i)recaptcha|hcaptcha|g-recaptcha|h-captcha`)

// LoginOutcome classifies the result of one synchronous portal login attempt.
// It is intentionally coarser than the full sync_run error taxonomy of ERD §8
// (CREDENTIAL_INVALID/CAPTCHA_BLOCKED/PORTAL_UNAVAILABLE/PARSING_BROKEN) —
// that taxonomy exists for the recurring background scraper connector (a later
// fatia); this synchronous, one-shot check only needs to answer the question
// the PUT endpoint asks: did it work, was it explicitly rejected, or is the
// outcome unknown?
type LoginOutcome int

const (
	// LoginOutcomeSuccess — the portal accepted the credential (the response
	// left the sso.tjsp.jus.br domain, the standard OIDC redirect back to
	// eproc1g.tjsp.jus.br after a successful authenticate).
	LoginOutcomeSuccess LoginOutcome = iota
	// LoginOutcomeRejected — the portal explicitly rejected the credential
	// (Keycloak's own "usuário ou senha inválida" markup). Confirmed live.
	LoginOutcomeRejected
	// LoginOutcomeInconclusive — the attempt did not reach a confident verdict:
	// a network/timeout fault, a captcha challenge, or markup that no longer
	// matches what was captured live (the portal's UI may have changed).
	LoginOutcomeInconclusive
)

// LoginResult is what Check returns: the classified outcome plus a short,
// human-readable detail — safe to store in portal_credential.last_error and
// show to the user, and safe to log (it never contains the password).
type LoginResult struct {
	Outcome LoginOutcome
	Detail  string
}

// PortalLoginChecker is the port the Configure use case depends on (domain.go)
// — never the concrete HTTP implementation. Check never returns a Go error for
// an inconclusive portal interaction (network faults, unexpected markup); those
// collapse into LoginOutcomeInconclusive so the use case's single call site
// stays simple: read Outcome, decide the status to persist. The only error
// return is for a cancelled/expired context — a caller-side fault, not a
// portal-side one.
type PortalLoginChecker interface {
	Check(ctx context.Context, login, password string) (LoginResult, error)
}

// TJSPEprocChecker is the PortalLoginChecker implementation for TJSP eproc
// (docs/erd-tribunal-scraping.md §5 Option B: plain HTTP, no headless
// browser). It is the ONLY portal this slice's v0 endpoints accept
// (PortalTJSPEproc), so the port carries no `portal` parameter.
type TJSPEprocChecker struct {
	client *http.Client
}

// checkerOption tunes a TJSPEprocChecker at construction — the same
// functional-options molde the DJEN connector uses for its own transport knobs.
type checkerOption func(*TJSPEprocChecker)

// WithHTTPClient overrides the default HTTP client — the seam a future uTLS+
// residential-proxy transport (mirroring djen.go's WAF-evasion, if eproc turns
// out to need it — see KNOWN_LIMITATIONS) would plug into, without touching the
// parsing/classification logic below.
func WithHTTPClient(c *http.Client) checkerOption {
	return func(t *TJSPEprocChecker) { t.client = c }
}

// NewTJSPEprocChecker builds a checker with a cookie-jar-backed client (the SSO
// flow's session_code/execution binding rides on Keycloak's own session
// cookies, captured live: KC_RESTART, AUTH_SESSION_ID) and a bounded timeout.
func NewTJSPEprocChecker(opts ...checkerOption) *TJSPEprocChecker {
	jar, _ := cookiejar.New(nil) // nil-Options New never errors; ignored per stdlib doc
	t := &TJSPEprocChecker{
		client: &http.Client{
			Jar:     jar,
			Timeout: tjspLoginTimeout,
		},
	}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

// Check runs the two-step login the live probe confirmed: GET the SSO start
// URL to obtain a fresh Keycloak login form + session cookies, then POST
// username/password to the form's own (session-bound) action URL. The verdict
// is read from where the final response landed and what its body contains —
// never from a fixed byte-for-byte page comparison, so cosmetic markup changes
// that don't touch the id="input-error"/domain shape still classify correctly.
func (t *TJSPEprocChecker) Check(ctx context.Context, login, password string) (LoginResult, error) {
	actionURL, result, done, err := t.fetchLoginFormAction(ctx)
	if err != nil {
		return LoginResult{}, err
	}
	if done {
		return result, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, actionURL,
		strings.NewReader(url.Values{"username": {login}, "password": {password}}.Encode()))
	if err != nil {
		if ctx.Err() != nil {
			return LoginResult{}, ctx.Err()
		}
		return LoginResult{Outcome: LoginOutcomeInconclusive, Detail: "erro interno ao montar a requisição de login"}, nil
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", tjspUserAgent)

	resp, err := t.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return LoginResult{}, ctx.Err()
		}
		return LoginResult{Outcome: LoginOutcomeInconclusive, Detail: "portal indisponível ou tempo esgotado ao enviar as credenciais"}, nil
	}
	defer resp.Body.Close()

	landedHost := resp.Request.URL.Host
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxLoginBodyBytes))

	return classifyLoginResponse(landedHost, body), nil
}

// maxLoginBodyBytes caps how much of the response we read — the login page is a
// few KB; a bound here is defensive against a portal fault streaming something
// unbounded back.
const maxLoginBodyBytes = 1 << 20 // 1 MiB

// fetchLoginFormAction performs the GET step and extracts the Keycloak login
// form's session-bound POST target, or (done=true) an already-final LoginResult
// when the GET itself fails or the expected form markup is absent. A cancelled
// or expired ctx is reported as ctxErr instead — the caller-side fault Check
// propagates as a real Go error, distinct from a portal-side inconclusive.
func (t *TJSPEprocChecker) fetchLoginFormAction(ctx context.Context) (actionURL string, result LoginResult, done bool, ctxErr error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tjspEprocLoginStartURL, nil)
	if err != nil {
		if ctx.Err() != nil {
			return "", LoginResult{}, true, ctx.Err()
		}
		return "", LoginResult{Outcome: LoginOutcomeInconclusive, Detail: "erro interno ao montar a requisição de login"}, true, nil
	}
	req.Header.Set("User-Agent", tjspUserAgent)

	resp, err := t.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", LoginResult{}, true, ctx.Err()
		}
		return "", LoginResult{Outcome: LoginOutcomeInconclusive, Detail: "portal indisponível ou tempo esgotado ao abrir a página de login"}, true, nil
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxLoginBodyBytes))

	match := kcFormActionRe.FindSubmatch(body)
	if match == nil {
		return "", LoginResult{
			Outcome: LoginOutcomeInconclusive,
			Detail:  "formulário de login do portal não encontrado (a estrutura da página pode ter mudado)",
		}, true, nil
	}

	return html.UnescapeString(string(match[1])), LoginResult{}, false, nil
}

// classifyLoginResponse reads the landing host and body of the POST response
// and returns the verdict. landedHost leaving the sso.tjsp.jus.br domain is the
// standard OIDC "authorization code" redirect back to the relying party
// (eproc1g.tjsp.jus.br) after Keycloak accepts the credential — the success
// signal. Staying on sso.tjsp.jus.br means Keycloak re-rendered something:
// either the explicit rejection markup (confirmed live), a captcha challenge,
// or an unrecognized page.
func classifyLoginResponse(landedHost string, body []byte) LoginResult {
	if !strings.Contains(landedHost, "sso.tjsp.jus.br") {
		return LoginResult{Outcome: LoginOutcomeSuccess}
	}
	if captchaMarkerRe.Match(body) {
		return LoginResult{
			Outcome: LoginOutcomeInconclusive,
			Detail:  "o portal exigiu verificação (captcha); não tentamos resolvê-la automaticamente",
		}
	}
	if kcLoginErrorRe.Match(body) {
		return LoginResult{Outcome: LoginOutcomeRejected, Detail: "usuário ou senha inválidos, segundo o portal"}
	}
	return LoginResult{
		Outcome: LoginOutcomeInconclusive,
		Detail:  "resposta do portal não reconhecida (a estrutura da página pode ter mudado)",
	}
}

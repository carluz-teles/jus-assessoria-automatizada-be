package eproc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"

	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// errCallbackTokenMissing signals Certisign's response carried no "cb" field — the
// auto-submit form (and its token) only appears when the certificate was accepted, so
// this means the certificate was rejected/not recognized, not a transport fault.
var errCallbackTokenMissing = errors.New("certisign: no callback token in response (certificate not accepted)")

// errX509NotAccepted signals certificadoSSORequest's response carried no
// "kc-x509-login-info" confirm form — Keycloak's X.509 authenticator only renders it
// after successfully reading a recognized certificate off the mTLS handshake.
var errX509NotAccepted = errors.New("eproc: certificate not accepted by Keycloak's X.509 authenticator")

// wiring.go isolates EVERYTHING provisional about the real eproc/Keycloak portal — the
// paths, the login request shape, the response parsing, and the challenge/success
// sniffing. It is the single place Portão B has to touch. Nothing here is verified
// against the live portal yet; each function is documented "ASSUMPTION (Portão B)".
//
// The MACHINE in eproc.go/auth.go (session reuse, re-auth on 401/redirect, streamed
// download, typed errors) is real and tested. This file is intentionally thin: we do
// NOT invest in elaborate HTML scraping we cannot verify.

const (
	// eprocDefaultBaseURL is the eproc 1º-grau portal root. ASSUMPTION (Portão B): the
	// real base and whether the JSON endpoints below exist must be confirmed. The SSO
	// itself is a separate host (sso.tjsp.jus.br) handled in ssoLoginRequest.
	eprocDefaultBaseURL = "https://eproc1g.tjsp.jus.br"

	// ssoLoginURL is the TJSP Keycloak login/token endpoint.
	//
	// CONFIRMED (Portão B, 2026-08-29, live probe against eproc1g.tjsp.jus.br/eproc/
	// with a real certificate): GETting the portal root redirects to
	// https://sso.tjsp.jus.br/realms/eproc/protocol/openid-connect/auth?response_type=code&...
	// — a real Keycloak, but realm "eproc" (not "tjsp" as originally guessed) and a
	// full browser AUTHORIZATION-CODE flow, NOT Resource-Owner-Password-Credentials.
	// The rendered login page's password form POSTs to
	// .../realms/eproc/login-actions/authenticate?session_code=...&execution=...
	// (both harvested from the page — ssoLoginRequest below is therefore STILL WRONG
	// for password mode; a ROPC POST to this constant will not work against the real
	// portal). Left uncorrected pending a password-mode Portão B pass; certificate
	// mode does not use this endpoint at all (see certLoginPath's doc — cert login is
	// brokered through Certisign, a separate discovery from the same probe).
	ssoLoginURL = "https://sso.tjsp.jus.br/realms/eproc/protocol/openid-connect/token"

	// browserUserAgent presents a current browser (the portal, like DJEN, edge-filters
	// bot-looking clients). It complements the Chrome uTLS transport the caller injects.
	browserUserAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"

	// challengePrefixBytes bounds how much of a login body we sniff for a captcha/MFA
	// marker — enough to catch a challenge page header without buffering a whole page.
	challengePrefixBytes = 8 << 10 // 8 KiB

	// ssoLoginBouncePath is what certLogin GETs first — CONFIRMED (Portão B,
	// 2026-08-29, real certificate + real CNJ, chromedp network capture) to be
	// exactly what eproc's OWN front-end JS requests when a route needs a Keycloak
	// session it doesn't have: it 302s to the Keycloak auth URL
	// (sso.tjsp.jus.br/realms/eproc/protocol/openid-connect/auth?kc_idp_hint=tjsp&
	// response_type=code&redirect_uri=.../SSO/callback&...&nonce=...&state=...),
	// which the client (following redirects normally) lands on.
	ssoLoginBouncePath = "/eproc/externo_controlador.php?acao=SSO/login&num_processo_bi=&lista_processos=&acao_origem=processo_selecionar"

	// certificadoSSOHost is the SEPARATE Keycloak vhost that actually gates on the
	// client certificate — CONFIRMED (Portão B): the login page's "Certificado
	// Digital" BUTTON (id="kc-login-certificate") runs
	// `document.location.host = 'certificado-sso.tjsp.jus.br'`, i.e. it re-issues
	// the CURRENT Keycloak auth URL (same path+query: kc_idp_hint, nonce, state,
	// redirect_uri — everything) against this different host. THAT host is where
	// the TLS layer actually requests/reads the client certificate (Keycloak's
	// built-in X.509 authenticator — confirmed by the "kc-x509-login-info" form
	// name in the response), landing on the OTP step next (every eproc login
	// requires TOTP per TJSP's manuals — ERD §11 item 3 already designed for this).
	//
	// This is DISTINCT from and MORE CORRECT than certisignLoginURL below: that one
	// is labeled on the real page itself "Acesso alternativo com certificado
	// digital" / "Utilize essa opção em caso de problema de acesso com o
	// certificado digital" — a documented FALLBACK that only grants the legacy
	// PHPSESSID-based session (acao=principal renders, but acao=processo_selecionar
	// still bounces to a fresh Keycloak login — verified live). The primary,
	// Keycloak-integrated path is this constant.
	certificadoSSOHost = "certificado-sso.tjsp.jus.br"

	// certisignLoginURL is the FALLBACK "Acesso alternativo com certificado
	// digital" link (see certificadoSSOHost's doc for why it is not the primary
	// path) — kept here, unused by certLogin today, in case the primary Keycloak
	// vhost is ever down and this documented fallback is worth wiring in too.
	// CONFIRMED (Portão B, 2026-08-29): GETting this with the client cert
	// configured on the transport (plain mutual TLS — no browser extension
	// involved; the extension's only job for a human is exposing the OS
	// certificate store to the TLS stack, which this package bypasses by
	// presenting the cert directly) makes Certisign read the cert off the mTLS
	// handshake and answer with an auto-submitting HTML form carrying a "cb"
	// token — see certisignCallbackPath's doc for what happens to it.
	certisignLoginURL = "https://autenticador.certisign.com.br/CertisignLogin/certificado/login" +
		"?id=28424&nome=eproc1g_prod" +
		"&retorno=" + eprocDefaultBaseURL + certisignCallbackPath

	// certisignCallbackPath is where the auto-submitting form from certisignLoginURL
	// posts its "cb" token — CONFIRMED (same probe) to establish the legacy eproc
	// session (a PHPSESSID cookie, landing on externo_controlador.php?acao=principal
	// only — see certificadoSSOHost's doc for why that is not enough on its own).
	certisignCallbackPath = "/eproc/externo_controlador.php?acao=entrar_certificado_certisign"
)

// eprocSearchPublicPath is the REAL quick-search entry point — CONFIRMED live
// 2026-08-31 (fetchProcessPage's doc has the full flow). cnj must already be
// formatCNJ'd.
func eprocSearchPublicPath(cnj string) string {
	return "/eproc/controlador.php?acao=processo_selecionar_publica&num_processo=" + url.QueryEscape(cnj)
}

// pesquisaRapidaActionRe extracts the quick-search form's action — SESSION-BOUND
// (the hash query param changes per login), so it must be re-extracted on every
// fetchProcessPage call, never cached across sessions.
var pesquisaRapidaActionRe = regexp.MustCompile(`id="formPesquisaRapida"\s*\n?\s*action="([^"]*)"`)

func extractPesquisaRapidaAction(body []byte) (string, error) {
	m := pesquisaRapidaActionRe.FindSubmatch(body)
	if m == nil {
		return "", errors.New("eproc: formPesquisaRapida action not found")
	}
	return strings.ReplaceAll(string(m[1]), "&amp;", "&"), nil
}

// formatCNJ normalizes a CNJ number to the dashed form eproc's search expects
// (NNNNNNN-DD.AAAA.J.TR.OOOO — CONFIRMED live 2026-08-31). Anything already
// containing a "-" is assumed pre-formatted and returned as-is; a raw 20-digit
// string (how internal/acquisition's court_record.cnj_number is stored,
// punctuation stripped on ingest) is reformatted. Anything else passes through
// unchanged rather than guessing.
func formatCNJ(raw string) string {
	if strings.Contains(raw, "-") {
		return raw
	}
	if len(raw) != 20 {
		return raw
	}
	return raw[0:7] + "-" + raw[7:9] + "." + raw[9:13] + "." + raw[13:14] + "." + raw[14:16] + "." + raw[16:20]
}

// eprocResolveDocPath turns a document link's href (a page-relative
// "controlador.php?acao=acessar_documento&…") into the absolute portal path
// doAuthed expects. eproc renders the event page under /eproc/, so a bare
// "controlador.php?…" resolves there; an href already rooted at "/" is verbatim.
func eprocResolveDocPath(href string) string {
	if strings.HasPrefix(href, "/") {
		return href
	}
	return "/eproc/" + href
}

// extractDocContentPath pulls the real content URL out of an acessar_documento
// wrapper: <iframe id="conteudoIframe" src="controlador.php?acao=acessar_documento_implementacao&…">.
// Returns "" when there is no such iframe (the wrapper IS the document — an inline
// HTML doc). attrVal returns the src with HTML entities (&amp;) already decoded.
func extractDocContentPath(wrapper []byte) string {
	root, err := html.Parse(bytes.NewReader(wrapper))
	if err != nil {
		return ""
	}
	iframe := findByID(root, "conteudoIframe")
	if iframe == nil {
		return ""
	}
	return attrVal(iframe, "src")
}

// applyBrowserHeaders sets the headers a browser would send, matching the anti-bot
// posture DJEN proved necessary. Never carries credentials.
func applyBrowserHeaders(req *http.Request) {
	req.Header.Set("User-Agent", browserUserAgent)
	req.Header.Set("Accept", "application/json, text/html, */*")
	req.Header.Set("Accept-Language", "pt-BR,pt;q=0.9,en;q=0.8")
}

// ssoLoginRequest builds the SSO login request. ASSUMPTION (Portão B): a Keycloak ROPC
// form POST — username/password + a placeholder client_id/grant_type. The credentials
// go in the POST body (form-encoded), NEVER in the URL or a header that gets logged. If
// the real flow is auth-code, this becomes a GET of the login page + a form POST with
// the harvested execution token; that change is contained to this one function.
func ssoLoginRequest(ctx context.Context, creds Credentials) (*http.Request, error) {
	form := url.Values{
		"grant_type": {"password"},
		"client_id":  {"eproc"}, // ASSUMPTION (Portão B): real client_id unknown
		"username":   {creds.Username},
		"password":   {creds.Password},
	}
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		ssoLoginURL,
		strings.NewReader(form.Encode()),
	)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req, nil
}

// ssoLoginBounceRequest builds the GET that starts the real certificate flow — see
// ssoLoginBouncePath's doc. The client's normal redirect-following lands this on the
// Keycloak auth URL for certificadoSSORequest to re-target at the cert-gated vhost.
func ssoLoginBounceRequest(ctx context.Context, baseURL string) (*http.Request, error) {
	return http.NewRequestWithContext(ctx, http.MethodGet, baseURL+ssoLoginBouncePath, nil)
}

// certificadoSSORequest re-issues the CURRENT Keycloak auth URL (kc_idp_hint, nonce,
// state, redirect_uri — everything the SSO bounce produced) against certificadoSSOHost
// instead of sso.tjsp.jus.br — exactly what the real "Certificado Digital" button's
// `document.location.host = 'certificado-sso.tjsp.jus.br'` does. The client certificate
// is presented during THIS request's TLS handshake (via the caller's transport).
func certificadoSSORequest(ctx context.Context, keycloakURL *url.URL) (*http.Request, error) {
	certURL := *keycloakURL
	certURL.Host = certificadoSSOHost
	return http.NewRequestWithContext(ctx, http.MethodGet, certURL.String(), nil)
}

// x509ConfirmFormActionRe extracts Keycloak's own "kc-x509-login-info" form action —
// CONFIRMED (Portão B): a hidden, auto-submitting confirmation Keycloak's built-in X.509
// authenticator renders after successfully reading the certificate off the mTLS
// handshake on certificadoSSOHost. Its action posts back to the ORIGINAL sso.tjsp.jus.br
// host (not certificadoSSOHost) to continue the OIDC flow. No token/HTML-parsing needed
// beyond this one attribute — the form has exactly one field (a submit button).
var x509ConfirmFormActionRe = regexp.MustCompile(`<form id="kc-x509-login-info"[^>]*action="([^"]*)"`)

// extractX509ConfirmAction pulls the confirm form's action URL out of
// certificadoSSORequest's response body, unescaping the "&amp;" HTML entities Keycloak's
// template emits in the (otherwise plain) query string. No match means Keycloak's X.509
// authenticator did NOT recognize the certificate — errX509NotAccepted, not a transport
// fault.
func extractX509ConfirmAction(body []byte) (string, error) {
	m := x509ConfirmFormActionRe.FindSubmatch(body)
	if m == nil {
		return "", errX509NotAccepted
	}
	return strings.ReplaceAll(string(m[1]), "&amp;", "&"), nil
}

// x509ConfirmRequest builds the POST that submits Keycloak's auto-submitting
// confirmation form — CONFIRMED (Portão B): a single "login=Continuar" field (the
// form's only input, a submit button), matching what the page's own inline JS
// (`document.getElementById("kc-x509-login-info").submit()`) does automatically in a
// real browser.
func x509ConfirmRequest(ctx context.Context, action string) (*http.Request, error) {
	form := url.Values{"login": {"Continuar"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, action, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req, nil
}

// errOTPFormMissing signals a challenge was detected (looksLikeChallenge matched) but
// no "kc-otp-login-form" was found to extract an action from — the page shape isn't
// what we expect, so submitOTP refuses to guess rather than POST somewhere wrong.
var errOTPFormMissing = errors.New("eproc: no OTP confirm form found in challenge response")

// otpConfirmFormActionRe extracts Keycloak's own "kc-otp-login-form" action —
// ASSUMPTION (Portão B): the form ID is Keycloak's own default theme naming (same
// family as the CONFIRMED "kc-x509-login-info"), so the shape is very likely right, but
// this specific form has never been submitted against the real portal yet. The next
// thing to confirm once TOTP enrollment (internal/court) has produced a real seed.
var otpConfirmFormActionRe = regexp.MustCompile(`<form id="kc-otp-login-form"[^>]*action="([^"]*)"`)

// extractOTPConfirmAction pulls the OTP form's action out of a challenge response body.
func extractOTPConfirmAction(body []byte) (string, error) {
	m := otpConfirmFormActionRe.FindSubmatch(body)
	if m == nil {
		return "", errOTPFormMissing
	}
	return strings.ReplaceAll(string(m[1]), "&amp;", "&"), nil
}

// otpConfirmRequest builds the POST that submits the generated code.
//
// ASSUMPTION (Portão B): the field name is Keycloak's own default OTP login template
// convention (`otp`), and the submit button mirrors kc-x509-login-info's
// ("login=Continuar" family). Isolated here so correcting the field name — the one
// real unknown left in this whole flow — is a one-line fix, same philosophy as every
// other wiring.go function.
func otpConfirmRequest(ctx context.Context, action, code string) (*http.Request, error) {
	form := url.Values{"otp": {code}, "login": {"Continuar"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, action, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req, nil
}

// cbFieldRe extracts the hidden "cb" field Certisign's auto-submitting form carries —
// CONFIRMED (Portão B): `<form ...><input type="hidden" id="cb" name="cb" value="...">`.
// A plain regex (not an HTML parser) is enough and matches the rest of this file's
// "don't invest in elaborate HTML scraping" philosophy — the field is a single opaque
// token, not a structure worth a DOM walk.
var cbFieldRe = regexp.MustCompile(`name="cb"\s+value="([^"]*)"`)

// certisignLoginRequest builds the GET that presents the client certificate to
// Certisign (mutual TLS, via the caller's transport — see certisignLoginURL's doc).
func certisignLoginRequest(ctx context.Context) (*http.Request, error) {
	return http.NewRequestWithContext(ctx, http.MethodGet, certisignLoginURL, nil)
}

// extractCallbackToken pulls the "cb" token out of Certisign's response body.
// ErrCallbackTokenMissing means Certisign did NOT recognize/accept the certificate —
// its auto-submit form (and therefore the token) only appears on success.
func extractCallbackToken(body []byte) (string, error) {
	m := cbFieldRe.FindSubmatch(body)
	if m == nil {
		return "", errCallbackTokenMissing
	}
	return string(m[1]), nil
}

// certisignCallbackRequest builds the POST that redeems the cb token for an eproc
// session — CONFIRMED (Portão B) to be a form POST (mirroring the browser's
// auto-submitted <form method="post">) to certisignCallbackPath. baseURL is the
// client's configured portal root (WithBaseURL), NOT the certisignLoginURL constant's
// hardcoded "retorno" param — that param is a fixed value Certisign was registered
// with in production and stays real even when a test points baseURL elsewhere.
func certisignCallbackRequest(ctx context.Context, baseURL, cb string) (*http.Request, error) {
	form := url.Values{"cb": {cb}}
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		baseURL+certisignCallbackPath,
		strings.NewReader(form.Encode()),
	)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req, nil
}

// loginSucceeded decides whether a 2xx/3xx login response is a real success vs the login
// page re-rendered with an error. ASSUMPTION (Portão B): a real success returns a token
// JSON or sets a session cookie; here we accept any non-error-marked 2xx/3xx. Confirm the
// real success signal against the portal.
func loginSucceeded(resp *http.Response, bodyPrefix []byte) bool {
	if resp.StatusCode >= 300 {
		return true // an OIDC redirect the client followed — treat as success
	}
	lower := strings.ToLower(string(bodyPrefix))
	// A re-rendered login page with an error marker is NOT success.
	return !strings.Contains(lower, "credenciais inválidas") &&
		!strings.Contains(lower, "invalid_grant") &&
		!strings.Contains(lower, "usuário ou senha")
}

// isLoginRedirect reports whether a redirect Location points at the SSO/login — the
// portal's way of saying "re-authenticate". Called by sessionRejected (the machine) to
// drive the automatic re-auth. ASSUMPTION (Portão B): matched by the "sso" / "login" /
// "auth" substrings; confirm the real redirect target against the portal.
func isLoginRedirect(location string) bool {
	if location == "" {
		return false
	}
	l := strings.ToLower(location)
	return strings.Contains(l, "sso.tjsp.jus.br") ||
		strings.Contains(l, "/login") ||
		strings.Contains(l, "/auth")
}

// looksLikeChallenge sniffs a body prefix for a captcha/MFA marker — the D0 UNKNOWN.
// ASSUMPTION (Portão B): matched by common markers; the real challenge shape must be
// confirmed. Returning true routes the caller to report a challenge distinctly.
func looksLikeChallenge(bodyPrefix []byte) bool {
	lower := strings.ToLower(string(bodyPrefix))
	markers := []string{
		"captcha",
		"recaptcha",
		"h-captcha",
		"autenticação de dois fatores",
		"two-factor",
		"código de verificação",
		"otp",
	}
	for _, m := range markers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// readBoundedPrefix reads up to challengePrefixBytes from r without consuming the whole
// (possibly huge) body — used only to sniff the login response for a challenge marker.
func readBoundedPrefix(r io.Reader) []byte {
	buf, _ := io.ReadAll(io.LimitReader(r, challengePrefixBytes))
	return buf
}

// --- Parsing (CONFIRMED live, 2026-08-31) ------------------------------------------
//
// The result page fetchProcessPage returns is REAL production HTML (not JSON — the
// portal has no API, only server-rendered pages meant for a browser). Parsed with
// golang.org/x/net/html (already a dependency — via otel/fiber transitively, not a
// new one) instead of regex: the cells are deeply nested (buttons, scripts, divs)
// in ways regex handles poorly. Parties ARE parsed now (parsePartiesHTML, below) off
// #tblPartesERepresentantes — CONFIRMED live: the capa exposes autor/réu, their
// CPF/CNPJ in clear, and their advogados (OAB), richer than DJEN.

// parseHTMLDocument parses an eproc page into an html tree, decoding its charset to
// UTF-8 first. eproc serves ISO-8859-1 (Latin-1) — accented values (magistrado,
// "Cível") arrive as raw Latin-1 bytes that are INVALID UTF-8, so parsing them without
// transcoding yields strings Postgres rejects on write. charset.NewReader sniffs the
// page's declared/heuristic encoding and transcodes to UTF-8 (a no-op for pages that
// really are UTF-8), so every extracted string is valid UTF-8.
func parseHTMLDocument(body []byte) (*html.Node, error) {
	reader, err := charset.NewReader(bytes.NewReader(body), "text/html")
	if err != nil {
		return nil, err
	}
	return html.Parse(reader)
}

// parseProcessHTML extracts the process capa metadata from the labeled spans the
// eproc detail page carries — CONFIRMED live 2026-08-31: #txtClasse (classe),
// #txtOrgaoJulgador (vara), #txtMagistrado (magistrado), #txtSituacao (situação,
// ex. "MOVIMENTO"), #txtCompetencia (competência) and #txtAutuacao (data/hora de
// autuação, "DD/MM/YYYY HH:MM:SS"). The page does NOT expose the valor da causa
// (only JS vars named *Valor*), so that field is intentionally absent. Each value
// is the span's text content; a missing span leaves its field zero-valued.
func parseProcessHTML(body []byte) (*Process, error) {
	root, err := parseHTMLDocument(body)
	if err != nil {
		return nil, fmt.Errorf("eproc: parse process html: %w", err)
	}
	text := func(id string) string {
		if n := findByID(root, id); n != nil {
			return strings.TrimSpace(textContent(n))
		}
		return ""
	}
	return &Process{
		Class:       text("txtClasse"),
		JudgingBody: text("txtOrgaoJulgador"),
		Magistrate:  text("txtMagistrado"),
		Situation:   text("txtSituacao"),
		Competence:  text("txtCompetencia"),
		FiledAt:     parseEproDateTime(text("txtAutuacao")),
		Parties:     parsePartiesFromRoot(root),
	}, nil
}

// cpfCnpjRe extracts a CPF/CNPJ from the capa's "( 284.669.278-59 ) - Pessoa Física"
// rendering — the fallback used when a party block carries no spnCpfParte* span (some
// rows render the document only inside the parenthesized text). Compiled once (regex
// compilation allocates; see the file's other package-level regexps).
var cpfCnpjRe = regexp.MustCompile(`\d{3}\.\d{3}\.\d{3}-\d{2}|\d{2}\.\d{3}\.\d{3}/\d{4}-\d{2}`)

// oabRe splits an eproc OAB rendering ("SP321511") into its UF ("SP") and number
// ("321511"): two letters followed by the registration digits. A value that doesn't
// match (already-formatted, foreign, empty) yields no split and is carried as-is in OAB.
var oabRe = regexp.MustCompile(`^([A-Za-z]{2})\s*(\d+)$`)

// parsePartiesFromRoot extracts the process's partes (autor/réu + CPF/CNPJ + advogados)
// off #tblPartesERepresentantes ONLY — the search is SCOPED to that table so the same
// data-parte attribute appearing elsewhere on the page (a <style> selector like
// span.infraEventoPrazoParte[data-parte="AUTOR"], or a docket row) is never mistaken
// for a party. Authors (AUTOR) are ordered before réus (REU). Returns an empty slice
// (never nil) when the table is absent, so callers can range freely.
func parsePartiesFromRoot(root *html.Node) []Party {
	table := findByID(root, "tblPartesERepresentantes")
	if table == nil {
		return []Party{}
	}

	markers := findAllByAttr(table, "data-parte")
	autores := make([]Party, 0, len(markers))
	reus := make([]Party, 0, len(markers))
	for _, marker := range markers {
		polo := strings.ToUpper(strings.TrimSpace(attrVal(marker, "data-parte")))
		if polo != "AUTOR" && polo != "REU" {
			continue // other polos (e.g. TERCEIRO) aren't split into cards yet
		}
		party := parsePartyFromMarker(marker, polo)
		if party.Name == "" {
			continue // a data-parte carrier with no name is decoration, not a party
		}
		if polo == "AUTOR" {
			autores = append(autores, party)
		} else {
			reus = append(reus, party)
		}
	}
	return append(dedupParties(autores), dedupParties(reus)...)
}

// dedupParties collapses parties that are the SAME participant rendered under more
// than one data-parte marker (eproc emits a marker per representation, so an autor
// with two procurações lands as two rows — e.g. "JOSE EDEN MACIEL" with counsels and
// "JOSé EDEN MACIEL" without). The key is the accent/case-insensitive name; entries
// merge: the richest one (a document, then counsels) is kept as the base and every
// other entry's counsels are unioned in (deduped by OAB|UF|name). Order is preserved
// by first appearance. Input is a single polo — callers dedup autores and réus
// separately so a homonym across polos is never merged.
func dedupParties(parties []Party) []Party {
	if len(parties) < 2 {
		return parties
	}
	index := make(map[string]int, len(parties))
	out := make([]Party, 0, len(parties))
	for _, p := range parties {
		key := normalizePartyName(p.Name)
		if i, ok := index[key]; ok {
			out[i] = mergeParties(out[i], p)
			continue
		}
		index[key] = len(out)
		out = append(out, p)
	}
	return out
}

// mergeParties folds b into a, keeping the richer identity: a document beats none,
// and (document being equal) the entry that already carries counsels stays the base.
// b's counsels are always unioned in so no advogado is lost regardless of which entry
// won. The winner's Name is kept verbatim (either grafia is acceptable to the FE).
func mergeParties(a, b Party) Party {
	base, extra := a, b
	if partyRichness(b) > partyRichness(a) {
		base, extra = b, a
	}
	base.Counsels = unionCounsels(base.Counsels, extra.Counsels)
	if base.Document == "" {
		base.Document = extra.Document
		base.RawDocument = extra.RawDocument
	}
	return base
}

// partyRichness ranks how much identifying data a party carries, so the merge keeps
// the fullest entry: a CPF/CNPJ is worth more than any number of counsels (identity
// over representation), then the presence of counsels breaks the tie.
func partyRichness(p Party) int {
	score := 0
	if p.Document != "" {
		score += 2
	}
	if len(p.Counsels) > 0 {
		score++
	}
	return score
}

// unionCounsels appends b's counsels to a, skipping any already present. Identity is
// OAB|UF when an OAB exists (the stable key), else the normalized name — so the same
// advogado read off two markers is not duplicated.
func unionCounsels(a, b []Counsel) []Counsel {
	seen := make(map[string]bool, len(a)+len(b))
	key := func(c Counsel) string {
		if c.OAB != "" {
			return "oab:" + strings.ToUpper(c.UF) + "|" + c.OAB
		}
		return "name:" + normalizePartyName(c.Name)
	}
	out := make([]Counsel, 0, len(a)+len(b))
	for _, c := range append(a, b...) {
		k := key(c)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, c)
	}
	return out
}

// normalizePartyName folds a party/advogado name to a comparison key: strip diacritics
// (NFD + drop combining marks), uppercase, and collapse whitespace. "JOSé EDEN MACIEL"
// and "JOSE EDEN  MACIEL" both key to "JOSE EDEN MACIEL". Mirrors draft's
// normalizeForMatch (a little copying over a cross-slice dependency).
func normalizePartyName(s string) string {
	t := transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	if out, _, err := transform.String(t, s); err == nil {
		s = out
	}
	return strings.Join(strings.Fields(strings.ToUpper(s)), " ")
}

// parsePartyFromMarker builds a Party from its name marker — the element that carries
// data-parte. The real capa puts data-parte on the <a class="infraNomeParte"> name link
// itself, so the CPF span and advogado <a>s are its FOLLOWING SIBLINGS (up to the next
// party marker), not its descendants. partyScope unites both regions so the parser works
// whether the capa nests a party's data under the marker or lays it out as siblings.
func parsePartyFromMarker(marker *html.Node, polo string) Party {
	scope := partyScope(marker)
	doc := documentInNodes(scope)
	return Party{
		Name:        partyNameFromMarker(marker),
		Role:        polo,
		Document:    doc,
		RawDocument: doc,
		Counsels:    counselsInNodes(scope),
	}
}

// partyScope returns the nodes that carry a party's data: the marker's own subtree
// (when the capa nests CPF/advogados under it) PLUS its following siblings until the
// next party marker (the real capa's layout, where they sit beside the name link). The
// decorations that PRECEDE the name link — the <style> .item-barra-atributos-parte block
// and the "Histórico de Representantes" icon link — are excluded, so neither can be read
// as party data.
func partyScope(marker *html.Node) []*html.Node {
	scope := []*html.Node{marker}
	for s := marker.NextSibling; s != nil; s = s.NextSibling {
		if isPartyMarker(s) {
			break // the next party starts here
		}
		scope = append(scope, s)
	}
	return scope
}

// isPartyMarker reports whether n is an AUTOR/REU party marker (an element carrying
// data-parte="AUTOR" or "REU").
func isPartyMarker(n *html.Node) bool {
	if n.Type != html.ElementNode {
		return false
	}
	polo := strings.ToUpper(strings.TrimSpace(attrVal(n, "data-parte")))
	return polo == "AUTOR" || polo == "REU"
}

// partyNameFromMarker reads the party's name: a spnNomeParte* span inside the marker
// when the capa renders one, otherwise the marker's own text up to the "( CPF )"
// decoration (the real capa puts the name as the <a> link's text).
func partyNameFromMarker(marker *html.Node) string {
	if span := findByIDPrefix(marker, "spnNomeParte"); span != nil {
		if name := strings.TrimSpace(textContent(span)); name != "" {
			return name
		}
	}
	raw := strings.TrimSpace(textContent(marker))
	if i := strings.IndexByte(raw, '('); i >= 0 {
		raw = raw[:i]
	}
	for _, line := range strings.Split(raw, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			return s
		}
	}
	return ""
}

// documentInNodes reads the party's CPF/CNPJ across its scope: first a spnCpfParte* span
// (the capa's dedicated field, e.g. spnCpfParteAutor0="284.669.278-59"), then the
// parenthesized "( 284.669.278-59 ) - Pessoa Física" text as a fallback. "" when neither
// is present.
func documentInNodes(nodes []*html.Node) string {
	for _, n := range nodes {
		if span := findByIDPrefix(n, "spnCpfParte"); span != nil {
			if doc := cpfCnpjRe.FindString(textContent(span)); doc != "" {
				return doc
			}
		}
	}
	for _, n := range nodes {
		if doc := cpfCnpjRe.FindString(textContent(n)); doc != "" {
			return doc
		}
	}
	return ""
}

// counselsInNodes collects every advogado across a party's scope: eproc renders each as
// an <a> flagged by infraTooltipMostrar('ADVOGADO',…) (or an adjacent sr-only div) whose
// content is the OAB ("UF+numero", e.g. "SP321511"); the advogado's NAME is the text
// immediately preceding that <a>. A scope with no such <a> yields an empty slice.
func counselsInNodes(nodes []*html.Node) []Counsel {
	var out []Counsel
	for _, n := range nodes {
		for _, a := range findAllAdvogadoLinks(n) {
			uf, oab := splitOAB(strings.TrimSpace(textContent(a)))
			out = append(out, Counsel{
				Name: nameBeforeNode(a),
				OAB:  oab,
				UF:   uf,
			})
		}
	}
	return out
}

// splitOAB splits eproc's "SP321511" into UF="SP", OAB="321511". A value that doesn't
// match the UF+digits shape is returned whole as the OAB with an empty UF, rather than
// guessing.
func splitOAB(raw string) (uf, oab string) {
	if m := oabRe.FindStringSubmatch(raw); m != nil {
		return strings.ToUpper(m[1]), m[2]
	}
	return "", raw
}

// parseEventsHTML extracts the docket (#tblEventos) — one row per movimentação,
// id="trEventoNN" (NN is the event's own sequence number, used as ExternalID: the
// portal exposes no other stable per-event id on this page). CONFIRMED structure:
// td[1]=número, td[2]=Data/Hora ("DD/MM/YYYY HH:MM:SS"), td[3] carries a
// label.infraEventoDescricao with the short description, td[5] carries zero or
// more a.infraLinkDocumento (href + data-doc/data-nome/data-mimetype attributes)
// or the literal text "Evento não gerou documento" when there is none. The href
// is the session-bound acessar_documento URL (see DocumentRef's doc) — captured
// whole because its key/hash can't be rebuilt from the doc id.
func parseEventsHTML(body []byte) ([]Event, error) {
	root, err := parseHTMLDocument(body)
	if err != nil {
		return nil, fmt.Errorf("eproc: parse events html: %w", err)
	}
	table := findByID(root, "tblEventos")
	if table == nil {
		return []Event{}, nil
	}

	rows := findEventRows(table)
	events := make([]Event, 0, len(rows))
	for _, row := range rows {
		tds := elementChildren(row, "td")
		if len(tds) < 5 {
			continue // malformed row — skip defensively rather than fail the whole read
		}

		number := strings.TrimSpace(textContent(tds[0]))
		when := parseEproDateTime(strings.TrimSpace(textContent(tds[1])))

		description := ""
		if descNode := findByClass(tds[2], "label", "infraEventoDescricao"); descNode != nil {
			description = strings.TrimSpace(textContent(descNode))
		}

		var docs []DocumentRef
		for _, a := range findAllByClass(tds[4], "a", "infraLinkDocumento") {
			docs = append(docs, DocumentRef{
				ExternalID:   attrVal(a, "data-doc"),
				DownloadPath: attrVal(a, "href"),
				Label:        attrVal(a, "data-nome"),
				MIMEType:     attrVal(a, "data-mimetype"),
			})
		}

		events = append(events, Event{
			ExternalID:  number,
			Date:        when,
			Description: description,
			Documents:   docs,
		})
	}
	return events, nil
}

// eprocTZ is where eproc's Data/Hora column is rendered in — the portal is TJSP's
// own system, always local Brazil time regardless of server location. Falls back
// to UTC (treating the wall-clock digits as-is) if the container has no tzdata —
// degraded but never a hard failure over a timezone database.
var eprocTZ = func() *time.Location {
	loc, err := time.LoadLocation("America/Sao_Paulo")
	if err != nil {
		return time.UTC
	}
	return loc
}()

func parseEproDateTime(s string) time.Time {
	t, err := time.ParseInLocation("02/01/2006 15:04:05", s, eprocTZ)
	if err != nil {
		return time.Time{}
	}
	return t
}

// --- Minimal HTML tree helpers (golang.org/x/net/html has no query API of its
// own — these are the handful of primitives parseProcessHTML/parseEventsHTML need,
// not a general-purpose selector engine). ---

func findByID(n *html.Node, id string) *html.Node {
	if n.Type == html.ElementNode && attrVal(n, "id") == id {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if found := findByID(c, id); found != nil {
			return found
		}
	}
	return nil
}

// findByIDPrefix returns the first descendant element whose id STARTS WITH prefix —
// eproc numbers per-party spans (spnCpfParteAutor0, spnNomeParteReu1, …), so an exact
// id can't be known ahead of time; the prefix is enough to find the one inside a given
// party block.
func findByIDPrefix(n *html.Node, prefix string) *html.Node {
	if n.Type == html.ElementNode && strings.HasPrefix(attrVal(n, "id"), prefix) {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if found := findByIDPrefix(c, prefix); found != nil {
			return found
		}
	}
	return nil
}

// findAllByAttr collects every descendant element (and n itself) that carries the
// attribute key with a non-empty value, in document order. Used to find party blocks
// by their data-parte attribute WITHIN the parties table — the scoping that keeps a
// data-parte in a <style> selector or a docket row from being read as a party.
func findAllByAttr(n *html.Node, key string) []*html.Node {
	var out []*html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && attrVal(n, key) != "" {
			out = append(out, n)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return out
}

// findAllAdvogadoLinks collects every <a> under n that eproc marks as an advogado —
// either by an infraTooltipMostrar('ADVOGADO',…) onmouseover or by an adjacent
// <div class="sr-only">Tipo de Usuário: ADVOGADO</div> (the two ways the capa flags
// the role). Other <a> links in the block (a party's own profile link, e.g.) are
// skipped so only real advogados become Counsels.
func findAllAdvogadoLinks(n *html.Node) []*html.Node {
	var out []*html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "a" && isAdvogadoLink(n) {
			out = append(out, n)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return out
}

// isAdvogadoLink reports whether an <a> is an advogado marker: its onmouseover invokes
// infraTooltipMostrar('ADVOGADO',…), or its FIRST following element sibling is a
// <div class="sr-only">Tipo de Usuário: ADVOGADO</div>. The sr-only check is bounded to
// the immediately-adjacent element — walking every following sibling let a party's own
// name <a> match a distant advogado's sr-only div and be read as a counsel (with the
// party name as OAB and the preceding <style> text as the name). An <a> that itself
// carries data-parte is the party's name link, never a counsel.
func isAdvogadoLink(a *html.Node) bool {
	if attrVal(a, "data-parte") != "" {
		return false
	}
	if strings.Contains(attrVal(a, "onmouseover"), "'ADVOGADO'") {
		return true
	}
	for s := a.NextSibling; s != nil; s = s.NextSibling {
		if s.Type != html.ElementNode {
			continue // skip the whitespace/&nbsp; text between </a> and the div
		}
		return hasClass(s, "sr-only") &&
			strings.Contains(strings.ToUpper(textContent(s)), "ADVOGADO")
	}
	return false
}

// nameBeforeNode returns the advogado's name: the trimmed text that immediately
// precedes the <a> element (the capa renders "PAULO SERGIO DE OLIVEIRA SOUZA" then the
// OAB <a>). It walks target's preceding text siblings, taking the last non-empty line.
func nameBeforeNode(target *html.Node) string {
	for s := target.PrevSibling; s != nil; s = s.PrevSibling {
		var text string
		switch s.Type {
		case html.TextNode:
			text = s.Data
		case html.ElementNode:
			if s.Data == "style" || s.Data == "script" {
				continue // never read CSS/JS as an advogado name
			}
			text = textContent(s)
		}
		lines := strings.Split(text, "\n")
		for i := len(lines) - 1; i >= 0; i-- {
			if line := strings.TrimSpace(lines[i]); line != "" {
				return line
			}
		}
	}
	return ""
}

// findEventRows collects every <tr id="trEventoNN"> under n, in document order.
func findEventRows(n *html.Node) []*html.Node {
	var out []*html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "tr" && strings.HasPrefix(attrVal(n, "id"), "trEvento") {
			out = append(out, n)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return out
}

// findByClass returns the first descendant <tag class="...class..."> under n.
func findByClass(n *html.Node, tag, class string) *html.Node {
	all := findAllByClass(n, tag, class)
	if len(all) == 0 {
		return nil
	}
	return all[0]
}

// findAllByClass collects every descendant <tag class="...class..."> under n, in
// document order (a node with several classes matches if any one of them equals
// class — same semantics as CSS's single-class selector).
func findAllByClass(n *html.Node, tag, class string) []*html.Node {
	var out []*html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == tag && hasClass(n, class) {
			out = append(out, n)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return out
}

func hasClass(n *html.Node, class string) bool {
	for _, f := range strings.Fields(attrVal(n, "class")) {
		if f == class {
			return true
		}
	}
	return false
}

// elementChildren returns n's DIRECT <tag> children only (not descendants) — used
// to walk a row's <td> cells in column order without descending into their content.
func elementChildren(n *html.Node, tag string) []*html.Node {
	var out []*html.Node
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && c.Data == tag {
			out = append(out, c)
		}
	}
	return out
}

func attrVal(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// textContent concatenates every text-node descendant of n, in document order —
// the DOM equivalent of a browser's textContent.
func textContent(n *html.Node) string {
	var sb strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			sb.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return sb.String()
}

package eproc

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

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

	// ssoLoginURL is the TJSP Keycloak login/token endpoint. ASSUMPTION (Portão B): a
	// real Keycloak realm exposes a token endpoint at
	// /realms/<realm>/protocol/openid-connect/token; the realm name and whether the
	// portal uses ROPC vs the browser auth-code flow must be confirmed. Kept as one
	// constant so it is a one-line fix.
	ssoLoginURL = "https://sso.tjsp.jus.br/realms/tjsp/protocol/openid-connect/token"

	// browserUserAgent presents a current browser (the portal, like DJEN, edge-filters
	// bot-looking clients). It complements the Chrome uTLS transport the caller injects.
	browserUserAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"

	// challengePrefixBytes bounds how much of a login body we sniff for a captcha/MFA
	// marker — enough to catch a challenge page header without buffering a whole page.
	challengePrefixBytes = 8 << 10 // 8 KiB
)

// eprocProcessPath/eventsPath/documentPath: ASSUMPTION (Portão B). Placeholder JSON
// endpoints so the machine has something to call in tests; the real portal paths (and
// whether they are JSON or HTML) must be confirmed.
func eprocProcessPath(escapedCNJ string) string {
	return "/api/processo/" + escapedCNJ
}

func eprocEventsPath(escapedCNJ string) string {
	return "/api/processo/" + escapedCNJ + "/eventos"
}

func eprocDocumentPath(escapedDocID string) string {
	return "/api/documento/" + escapedDocID
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

// --- Parsing (ASSUMPTION, Portão B) ------------------------------------------------
//
// The parsers below decode a PLACEHOLDER JSON shape so the machine and its tests have a
// concrete contract. The real eproc responses (JSON or HTML) are unknown until Portão B;
// when they arrive, only these parsers change. RawDocument is carried verbatim — the
// whole point of D0 is to discover whether the portal exposes CPF/CNPJ in clear.

type processDTO struct {
	CNJNumber   string     `json:"numero_cnj"`
	Class       string     `json:"classe"`
	JudgingBody string     `json:"orgao_julgador"`
	Parties     []partyDTO `json:"partes"`
}

type partyDTO struct {
	Name        string `json:"nome"`
	Role        string `json:"polo"`
	RawDocument string `json:"documento"` // CPF/CNPJ exactly as the portal returns it
}

type eventDTO struct {
	ExternalID  string        `json:"id"`
	Date        string        `json:"data"`
	Description string        `json:"descricao"`
	Documents   []documentDTO `json:"documentos"`
}

type documentDTO struct {
	ExternalID string `json:"id"`
	Label      string `json:"rotulo"`
	MIMEType   string `json:"mime_type"`
}

func parseProcess(r io.Reader) (*Process, error) {
	var dto processDTO
	if err := json.NewDecoder(r).Decode(&dto); err != nil {
		return nil, err
	}
	parties := make([]Party, 0, len(dto.Parties))
	for _, p := range dto.Parties {
		parties = append(parties, Party{
			Name:        p.Name,
			Role:        p.Role,
			RawDocument: p.RawDocument,
		})
	}
	return &Process{
		CNJNumber:   dto.CNJNumber,
		Class:       dto.Class,
		JudgingBody: dto.JudgingBody,
		Parties:     parties,
	}, nil
}

func parseEvents(r io.Reader) ([]Event, error) {
	var dtos []eventDTO
	if err := json.NewDecoder(r).Decode(&dtos); err != nil {
		return nil, err
	}
	events := make([]Event, 0, len(dtos))
	for _, d := range dtos {
		var when time.Time
		if d.Date != "" {
			// ASSUMPTION (Portão B): ISO-8601; the real date format must be confirmed. A
			// parse failure leaves the zero time rather than failing the whole read.
			if t, err := time.Parse(time.RFC3339, d.Date); err == nil {
				when = t
			}
		}
		docs := make([]DocumentRef, 0, len(d.Documents))
		for _, dc := range d.Documents {
			docs = append(docs, DocumentRef{
				ExternalID: dc.ExternalID,
				Label:      dc.Label,
				MIMEType:   dc.MIMEType,
			})
		}
		events = append(events, Event{
			ExternalID:  d.ExternalID,
			Date:        when,
			Description: d.Description,
			Documents:   docs,
		})
	}
	return events, nil
}

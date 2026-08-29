package eproc

import (
	"context"
	"io"
	"net/http"
	"sync"

	"github.com/jusassessoria/platform/lib/apperr"
)

// authGroup single-flights concurrent logins: when a burst of read calls all find the
// session stale at once, exactly one login runs and the rest wait on its result. This
// avoids hammering the SSO with N parallel logins (and tripping its own rate limits).
type authGroup struct {
	mu      sync.Mutex
	pending *authCall
}

type authCall struct {
	done chan struct{}
	err  error
}

func newAuthGroup() *authGroup {
	return &authGroup{}
}

// authGroupLogin runs c.login under the single-flight group: the first caller executes
// the login, concurrent callers block on the same result.
func (c *Client) authGroupLogin(ctx context.Context) error {
	g := c.authOnce

	g.mu.Lock()
	if g.pending != nil {
		call := g.pending
		g.mu.Unlock()
		// Wait for the in-flight login, but honor this caller's cancellation.
		select {
		case <-call.done:
			return call.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	call := &authCall{done: make(chan struct{})}
	g.pending = call
	g.mu.Unlock()

	call.err = c.login(ctx)

	g.mu.Lock()
	g.pending = nil
	g.mu.Unlock()
	close(call.done)

	return call.err
}

// login performs the SSO authentication and, on success, marks the client CONNECTED.
// The cookie jar captures the session cookies for reuse. It dispatches to
// passwordLogin or certLogin per c.mode — see authMode's doc for why these are
// genuinely different mechanisms, not two code paths for the same request.
func (c *Client) login(ctx context.Context) error {
	if c.mode == authModeCertificate {
		return c.certLogin(ctx)
	}
	return c.passwordLogin(ctx)
}

// passwordLogin performs the SSO authentication against the Keycloak instance.
//
// ASSUMPTION (Portão B): the exact login is a Keycloak Resource-Owner-Password-style
// form POST to the SSO token/login endpoint with the username/password form fields. The
// real portal may instead require fetching a login page to harvest a CSRF/execution
// token first, or an OIDC redirect dance. That wiring is isolated in ssoLoginRequest so
// correcting it is a small, contained change. What is REAL and tested here: on 200 we
// become CONNECTED; a 401/403 is a credential/challenge failure; a captcha marker in the
// body is surfaced distinctly; a transport error is Unavailable.
func (c *Client) passwordLogin(ctx context.Context) error {
	if c.creds.Username == "" || c.creds.Password == "" {
		return apperr.NewInvalid("eproc: missing credentials")
	}

	req, err := ssoLoginRequest(ctx, c.creds)
	if err != nil {
		return apperr.NewInfra("eproc: build login request", err)
	}
	applyBrowserHeaders(req)

	resp, err := c.hc.Do(req)
	if err != nil {
		return apperr.NewUnavailable("eproc: login request failed", err)
	}
	defer resp.Body.Close()

	// Read a bounded prefix of the body so we can sniff for a captcha/MFA challenge
	// without buffering an arbitrarily large page.
	prefix := readBoundedPrefix(resp.Body)

	if looksLikeChallenge(prefix) {
		// The spike's second UNKNOWN: did a captcha/MFA appear? Surface it as Forbidden
		// (access blocked by a challenge, not wrong password) so the caller reports it.
		return apperr.NewForbidden("eproc: login blocked by captcha/MFA challenge")
	}

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return apperr.NewUnauthorized("eproc: login rejected (invalid credentials)")
	case resp.StatusCode >= 500:
		return apperr.NewUnavailable("eproc: SSO unavailable", statusError(resp.StatusCode))
	case resp.StatusCode >= 200 && resp.StatusCode < 400:
		// 2xx or an OIDC redirect the jar/client already followed — treat as success and
		// let the first read confirm the session. loginSucceeded lets the wiring reject a
		// 200 that is actually the login page re-rendered with an error.
		if !loginSucceeded(resp, prefix) {
			return apperr.NewUnauthorized("eproc: login rejected (login page re-rendered)")
		}
		c.markConnected()
		return nil
	default:
		return apperr.NewUnavailable("eproc: unexpected SSO status", statusError(resp.StatusCode))
	}
}

// certLogin performs the real certificate login CONFIRMED end-to-end against the live
// portal (Portão B, 2026-08-29, real certificate + real CNJ, verified up to the point
// this package can go without a TOTP seed — see WithCertificateAuth's doc):
//
//  1. GET ssoLoginBouncePath and let the client follow eproc's redirect to Keycloak's
//     auth URL (kc_idp_hint, nonce, state, redirect_uri) — exactly what eproc's own
//     front-end JS does when a route needs a Keycloak session.
//  2. Re-issue that SAME URL against certificadoSSOHost (certificadoSSORequest) — the
//     client certificate is presented during THIS request's TLS handshake (never by
//     this function — see authMode's doc). Keycloak's built-in X.509 authenticator
//     reads it and, on success, renders a hidden "kc-x509-login-info" confirm form.
//     No form found means the certificate was rejected (errX509NotAccepted) —
//     Unauthorized, not Unavailable.
//  3. POST that form (x509ConfirmRequest) — what the page's own auto-submit JS does.
//
// A captcha/MFA marker in the final response is the TOTP step TJSP's own manuals
// document as mandatory after certificate recognition (confirmed live: Keycloak's next
// step here is literally a "kc-otp-login-form") — surfaced as Forbidden (certificate
// accepted, second factor needed), not a login failure. Completing that TOTP step
// needs the seed captured at onboarding (ERD §11 item 3) — out of scope for this
// package until court_connection exists.
func (c *Client) certLogin(ctx context.Context) error {
	bounce, err := ssoLoginBounceRequest(ctx, c.baseURL)
	if err != nil {
		return apperr.NewInfra("eproc: build SSO bounce request", err)
	}
	applyBrowserHeaders(bounce)

	bounceResp, err := c.hc.Do(bounce)
	if err != nil {
		return apperr.NewUnavailable("eproc: SSO bounce request failed", err)
	}
	keycloakURL := bounceResp.Request.URL
	io.Copy(io.Discard, bounceResp.Body) //nolint:errcheck
	bounceResp.Body.Close()

	step1, err := certificadoSSORequest(ctx, keycloakURL)
	if err != nil {
		return apperr.NewInfra("eproc: build certificado-sso request", err)
	}
	applyBrowserHeaders(step1)

	resp1, err := c.hc.Do(step1)
	if err != nil {
		return apperr.NewUnavailable("eproc: certificado-sso request failed", err)
	}
	body1, err := io.ReadAll(resp1.Body)
	resp1.Body.Close()
	if err != nil {
		return apperr.NewUnavailable("eproc: read certificado-sso response", err)
	}

	action, err := extractX509ConfirmAction(body1)
	if err != nil {
		return apperr.NewUnauthorized("eproc: certificate rejected by Keycloak's X.509 authenticator (not recognized or expired)")
	}

	step2, err := x509ConfirmRequest(ctx, action)
	if err != nil {
		return apperr.NewInfra("eproc: build X.509 confirm request", err)
	}
	applyBrowserHeaders(step2)

	resp2, err := c.hc.Do(step2)
	if err != nil {
		return apperr.NewUnavailable("eproc: X.509 confirm failed", err)
	}
	defer resp2.Body.Close()

	prefix := readBoundedPrefix(resp2.Body)

	if looksLikeChallenge(prefix) {
		return apperr.NewForbidden("eproc: certificate accepted, MFA/TOTP challenge required")
	}

	switch {
	case resp2.StatusCode == http.StatusUnauthorized || resp2.StatusCode == http.StatusForbidden:
		return apperr.NewUnauthorized("eproc: X.509 confirm rejected by eproc")
	case resp2.StatusCode >= 500:
		return apperr.NewUnavailable("eproc: portal unavailable", statusError(resp2.StatusCode))
	case resp2.StatusCode >= 200 && resp2.StatusCode < 400:
		if isLoginRedirect(resp2.Header.Get("Location")) || !loginSucceeded(resp2, prefix) {
			return apperr.NewUnauthorized("eproc: X.509 confirm did not establish a session (redirected back to login)")
		}
		c.markConnected()
		return nil
	default:
		return apperr.NewUnavailable("eproc: unexpected portal status", statusError(resp2.StatusCode))
	}
}

// certLoginCertisignFallback is the "Acesso alternativo com certificado digital"
// fallback path (see certisignLoginURL's doc) — NOT called by certLogin today (it only
// grants a legacy PHPSESSID-based session, not the full Keycloak-integrated one), kept
// so it's a small change to wire in if the primary certificadoSSOHost vhost is ever
// unavailable. Same two-hop shape: GET certisignLoginURL (cert presented via mTLS),
// extract the "cb" token, POST it to certisignCallbackPath.
func (c *Client) certLoginCertisignFallback(ctx context.Context) error {
	step1, err := certisignLoginRequest(ctx)
	if err != nil {
		return apperr.NewInfra("eproc: build certisign request", err)
	}
	applyBrowserHeaders(step1)

	resp1, err := c.hc.Do(step1)
	if err != nil {
		return apperr.NewUnavailable("eproc: certisign request failed", err)
	}
	body1, err := io.ReadAll(io.LimitReader(resp1.Body, challengePrefixBytes))
	resp1.Body.Close()
	if err != nil {
		return apperr.NewUnavailable("eproc: read certisign response", err)
	}

	cb, err := extractCallbackToken(body1)
	if err != nil {
		return apperr.NewUnauthorized("eproc: certificate rejected by certisign (not recognized or expired)")
	}

	step2, err := certisignCallbackRequest(ctx, c.baseURL, cb)
	if err != nil {
		return apperr.NewInfra("eproc: build certisign callback request", err)
	}
	applyBrowserHeaders(step2)

	resp2, err := c.hc.Do(step2)
	if err != nil {
		return apperr.NewUnavailable("eproc: certisign callback failed", err)
	}
	defer resp2.Body.Close()

	prefix := readBoundedPrefix(resp2.Body)

	if looksLikeChallenge(prefix) {
		return apperr.NewForbidden("eproc: certificate accepted, MFA/TOTP challenge required")
	}

	switch {
	case resp2.StatusCode == http.StatusUnauthorized || resp2.StatusCode == http.StatusForbidden:
		return apperr.NewUnauthorized("eproc: certisign callback rejected by eproc")
	case resp2.StatusCode >= 500:
		return apperr.NewUnavailable("eproc: portal unavailable", statusError(resp2.StatusCode))
	case resp2.StatusCode >= 200 && resp2.StatusCode < 400:
		if isLoginRedirect(resp2.Header.Get("Location")) || !loginSucceeded(resp2, prefix) {
			return apperr.NewUnauthorized("eproc: certisign callback did not establish a session (redirected back to login)")
		}
		c.markConnected()
		return nil
	default:
		return apperr.NewUnavailable("eproc: unexpected portal status", statusError(resp2.StatusCode))
	}
}

// sessionRejected reports whether a response means "your session is no longer valid" —
// a 401, or a redirect whose Location points back at the login/SSO. This is what drives
// the automatic re-auth in doAuthed.
func sessionRejected(resp *http.Response) bool {
	if resp.StatusCode == http.StatusUnauthorized {
		return true
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		loc := resp.Header.Get("Location")
		// isLoginRedirect is a wiring predicate (Portão B): the portal-specific redirect
		// target lives in wiring.go so the machine here stays portal-agnostic.
		return isLoginRedirect(loc)
	}
	return false
}

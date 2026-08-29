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

// certLogin performs the real two-hop certificate login CONFIRMED against the live
// portal (Portão B, 2026-08-29, real certificate + real CNJ):
//
//  1. GET certisignLoginURL. The client certificate is presented during the TLS
//     handshake itself (never by this function — see authMode's doc); Certisign reads
//     it off the mTLS session and, on acceptance, answers with an auto-submitting HTML
//     form carrying a "cb" token. No token in the body means the certificate was
//     rejected (errCallbackTokenMissing) — surfaced as Unauthorized, not Unavailable.
//  2. POST that token to certisignCallbackPath — the same request the form's onload
//     JS would auto-submit in a real browser. On success this lands the client on
//     eproc's authenticated home (acao=principal) with a real session cookie.
//
// A captcha/MFA marker in the final response is the TOTP step TJSP's own manuals
// document as mandatory after certificate recognition — surfaced as Forbidden
// (certificate accepted, second factor needed) rather than a login failure.
func (c *Client) certLogin(ctx context.Context) error {
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

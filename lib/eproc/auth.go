package eproc

import (
	"context"
	"io"
	"net/http"
	"sync"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/totp"
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
// become CONNECTED; a 401/403 is a credential/challenge failure; a transport error is
// Unavailable. A captcha/MFA marker is handled by classifyLoginResult — see its doc.
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
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return apperr.NewUnavailable("eproc: read login response", err)
	}

	return c.classifyLoginResult(ctx, resp, body, false)
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
// step here is literally a "kc-otp-login-form") — handled by classifyLoginResult: with
// a seed configured (WithTOTPSeed) it completes automatically; without one it surfaces
// Forbidden (certificate accepted, second factor needed — the caller's cue to run MFA
// enrollment once, see internal/court).
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
	body2, err := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if err != nil {
		return apperr.NewUnavailable("eproc: read X.509 confirm response", err)
	}

	return c.classifyLoginResult(ctx, resp2, body2, false)
}

// classifyLoginResult is the shared tail of every login mechanism once it has a final
// HTTP response + body in hand: a captcha/MFA marker in the body is the TOTP step TJSP's
// own manuals document as mandatory after certificate/password recognition (confirmed
// live: Keycloak's next step is literally a "kc-otp-login-form"). When a TOTP seed is
// configured (WithTOTPSeed — captured once at MFA enrollment, see internal/court) and
// this is the first time this login attempt has seen the challenge, submitOTP generates
// the code and completes the flow automatically instead of surfacing Forbidden — this
// is the whole point of court_connection's mfa_seed_ref: no human, no phone, ever again
// after the one-time enrollment. otpAttempted guards against looping if the OTP
// submission itself lands on ANOTHER challenge (wrong/expired seed, rate limit).
func (c *Client) classifyLoginResult(ctx context.Context, resp *http.Response, body []byte, otpAttempted bool) error {
	if looksLikeChallenge(body) {
		if c.totpSeed != "" && !otpAttempted {
			nextResp, nextBody, err := c.submitOTP(ctx, body)
			if err != nil {
				return err
			}
			return c.classifyLoginResult(ctx, nextResp, nextBody, true)
		}
		return apperr.NewForbidden("eproc: certificate accepted, MFA/TOTP challenge required")
	}

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return apperr.NewUnauthorized("eproc: login rejected by eproc")
	case resp.StatusCode >= 500:
		return apperr.NewUnavailable("eproc: portal unavailable", statusError(resp.StatusCode))
	case resp.StatusCode >= 200 && resp.StatusCode < 400:
		// 2xx or an OIDC redirect the jar/client already followed — treat as success and
		// let the first read confirm the session. loginSucceeded lets the wiring reject a
		// 200 that is actually the login page re-rendered with an error.
		if isLoginRedirect(resp.Header.Get("Location")) || !loginSucceeded(resp, body) {
			return apperr.NewUnauthorized("eproc: login did not establish a session (redirected back to login)")
		}
		c.markConnected()
		return nil
	default:
		return apperr.NewUnavailable("eproc: unexpected portal status", statusError(resp.StatusCode))
	}
}

// submitOTP generates the 6-digit code for c.totpSeed and posts it to the OTP
// confirmation form found in body — see extractOTPConfirmAction/otpConfirmRequest
// (wiring.go) for what is ASSUMPTION (Portão B) vs confirmed. No form found means the
// challenge wasn't the OTP page we expect (errOTPFormMissing) — surfaced as Unauthorized,
// not silently ignored.
func (c *Client) submitOTP(ctx context.Context, body []byte) (*http.Response, []byte, error) {
	action, err := extractOTPConfirmAction(body)
	if err != nil {
		return nil, nil, apperr.NewUnauthorized("eproc: MFA challenge present but no OTP form found (page shape changed?)")
	}

	code, err := totp.GenerateCode(c.totpSeed, c.now())
	if err != nil {
		return nil, nil, apperr.NewInfra("eproc: generate TOTP code", err)
	}

	req, err := otpConfirmRequest(ctx, action, code)
	if err != nil {
		return nil, nil, apperr.NewInfra("eproc: build OTP confirm request", err)
	}
	applyBrowserHeaders(req)

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, nil, apperr.NewUnavailable("eproc: OTP confirm request failed", err)
	}
	respBody, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, nil, apperr.NewUnavailable("eproc: read OTP confirm response", err)
	}
	return resp, respBody, nil
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

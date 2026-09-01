package eproc

import (
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/jusassessoria/platform/lib/apperr"
)

// Session is an opaque, serializable snapshot of a Client's cookies — the thing a
// caller persists (e.g. sealed in a vault, internal/court's court_connection.session_ref)
// to skip a full cert+TOTP login on a LATER, independent Client instance. Asynq
// workers are stateless/horizontal: an in-memory cookie jar never survives across
// task executions or replicas, so the session itself has to be the persisted unit,
// not just the Client holding it.
type Session []byte

// sessionCookies is the JSON shape: one entry per origin, since cookiejar.Jar scopes
// cookies per URL and has no "dump everything" method.
type sessionCookies struct {
	Host    string         `json:"host"`
	Cookies []*http.Cookie `json:"cookies"`
}

// sessionHosts are exactly the three origins certLogin's flow touches (auth.go,
// wiring.go's ssoLoginBouncePath/certificadoSSOHost) — each may hold a cookie the
// eproc app or Keycloak needs on the next request. Kept as a function (not a
// package var) of baseURL so a test pointed at an httptest server exports/imports
// against ITS host instead of the hardcoded production ones.
func sessionHosts(baseURL string) []string {
	return []string{
		baseURL,
		"https://sso.tjsp.jus.br",
		"https://" + certificadoSSOHost,
	}
}

// ExportSession snapshots the client's current cookies for the login flow's hosts.
// Call it unconditionally after any successful authenticated call, not only after an
// explicit Login() — doAuthed can silently re-authenticate mid-request on a stale
// session, and only exporting afterward captures a session renewed that way.
//
// Note this is NOT a byte-faithful cookie dump: cookiejar.Jar.Cookies only returns
// Name/Value (what a Cookie request header needs), so Expires/Domain/Secure/HttpOnly
// are lost on round-trip — WithSession reimports these as host-scoped session
// cookies. Harmless here: server-side validity (not the jar's own expiry) is what
// doAuthed's sessionRejected check actually relies on.
func (c *Client) ExportSession() (Session, error) {
	if c.hc.Jar == nil {
		return nil, nil
	}

	snap := make([]sessionCookies, 0, len(sessionHosts(c.baseURL)))
	for _, host := range sessionHosts(c.baseURL) {
		u, err := url.Parse(host)
		if err != nil {
			continue // unreachable for the fixed hosts above; skip rather than fail the export
		}
		if cookies := c.hc.Jar.Cookies(u); len(cookies) > 0 {
			snap = append(snap, sessionCookies{Host: host, Cookies: cookies})
		}
	}

	b, err := json.Marshal(snap)
	if err != nil {
		return nil, apperr.NewInfra("eproc: marshal session", err)
	}
	return Session(b), nil
}

// WithSession primes a freshly-constructed Client with a previously exported
// session, skipping the login round-trip if it is still valid server-side —
// doAuthed's existing re-auth-on-401/redirect machinery transparently falls back to
// a full login if it is not. An empty or corrupted session is a silent no-op: a
// stale/garbled persisted blob must never fail the constructor, it just falls
// through to a normal login on the first authenticated call.
func WithSession(s Session) Option {
	return func(c *Client) {
		if len(s) == 0 || c.hc.Jar == nil {
			return
		}
		var snap []sessionCookies
		if err := json.Unmarshal(s, &snap); err != nil {
			return
		}

		imported := false
		for _, entry := range snap {
			if len(entry.Cookies) == 0 {
				continue
			}
			u, err := url.Parse(entry.Host)
			if err != nil {
				continue
			}
			c.hc.Jar.SetCookies(u, entry.Cookies)
			imported = true
		}
		if imported {
			// Optimistic: a primed session is presumptively usable, so the first read
			// skips a needless proactive login. If it's actually stale, doAuthed's
			// sessionRejected check catches it on the first response and re-authenticates.
			c.status = StatusConnected
		}
	}
}

package eproc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// TestClient_ExportSession_SkipsReloginOnFreshClient proves the whole point of
// session persistence: a SECOND, independent Client primed via WithSession reuses
// the first Client's session instead of logging in again — the mechanic
// worker-court's FetchAutosUseCase relies on to avoid a login per task execution.
//
// The jar is seeded directly (not via the password-login flow) because
// newTestClient's rewriteTransport swaps the wire destination but NOT the request's
// own URL that http.Client.send uses for cookie-jar bookkeeping (cookieURL :=
// req.URL, computed before RoundTrip runs) — in this test harness that would scope
// the login cookie under "sso.tjsp.jus.br", not the stub server's own host, which
// is an artifact of the harness's URL-rewriting trick, not of ExportSession/
// WithSession's actual contract (which this test targets directly).
func TestClient_ExportSession_SkipsReloginOnFreshClient(t *testing.T) {
	t.Parallel()

	stub := &stubPortal{}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	c1 := newTestClient(t, srv, validCreds())
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse srv.URL: %v", err)
	}
	c1.hc.Jar.SetCookies(u, []*http.Cookie{{Name: "eproc_session", Value: "abc"}})
	c1.markConnected()

	session, err := c1.ExportSession()
	if err != nil {
		t.Fatalf("ExportSession: %v", err)
	}
	if len(session) == 0 {
		t.Fatal("ExportSession returned an empty session after seeding a cookie")
	}

	hc2 := &http.Client{Transport: &rewriteTransport{base: http.DefaultTransport, server: srv.URL}}
	c2 := NewEprocClient(hc2, WithBaseURL(srv.URL), WithCredentials(validCreds()), WithSession(session))

	if c2.Status() != StatusConnected {
		t.Errorf("client primed with a session should start CONNECTED, got %q", c2.Status())
	}

	ctx := context.Background()
	if _, err := c2.GetProcess(ctx, "x"); err != nil {
		t.Fatalf("GetProcess (client 2, primed session): %v", err)
	}
	if got := stub.logins.Load(); got != 0 {
		t.Errorf("logins after client 2 reused the session = %d, want 0 (session avoided any login)", got)
	}
}

// TestClient_ExportSession_BeforeLoginReturnsEmptySession proves exporting before
// any login never errors and produces a session with nothing meaningful to import.
func TestClient_ExportSession_BeforeLoginReturnsEmptySession(t *testing.T) {
	t.Parallel()

	stub := &stubPortal{}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	c := newTestClient(t, srv, validCreds())
	session, err := c.ExportSession()
	if err != nil {
		t.Fatalf("ExportSession before any login: %v", err)
	}

	hc2 := &http.Client{Transport: &rewriteTransport{base: http.DefaultTransport, server: srv.URL}}
	c2 := NewEprocClient(hc2, WithBaseURL(srv.URL), WithCredentials(validCreds()), WithSession(session))
	if c2.Status() != StatusDisconnected {
		t.Errorf("status = %q, want DISCONNECTED (empty session has no cookies to import)", c2.Status())
	}
}

// TestWithSession_EmptyOrCorruptedIsNoOp proves a stale/garbled persisted blob never
// breaks the constructor — it must silently fall through to a normal login instead.
func TestWithSession_EmptyOrCorruptedIsNoOp(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		session Session
	}{
		{"nil", nil},
		{"empty", Session{}},
		{"corrupted json", Session("not json")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			stub := &stubPortal{}
			srv := httptest.NewServer(stub.handler())
			defer srv.Close()

			hc := &http.Client{Transport: &rewriteTransport{base: http.DefaultTransport, server: srv.URL}}
			c := NewEprocClient(hc, WithBaseURL(srv.URL), WithCredentials(validCreds()), WithSession(tt.session))

			if c.Status() != StatusDisconnected {
				t.Errorf("status = %q, want DISCONNECTED (must fall through to normal login)", c.Status())
			}
			if _, err := c.GetProcess(context.Background(), "x"); err != nil {
				t.Fatalf("GetProcess: %v", err)
			}
			if got := stub.logins.Load(); got != 1 {
				t.Errorf("logins = %d, want 1 (normal login must still happen)", got)
			}
		})
	}
}

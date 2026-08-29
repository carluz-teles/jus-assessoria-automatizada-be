package eproc

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// stubPortal is an httptest-backed fake of the eproc portal + SSO. It counts logins and
// data hits, and can be told to reject the session once (to exercise re-auth) or to
// return a specific status for the data endpoints (to exercise typed errors). The SSO
// login endpoint and the data endpoints share one server for test simplicity: the login
// URL is absolute (ssoLoginURL) so we override it via a rewriting transport.
type stubPortal struct {
	logins   atomic.Int32
	dataHits atomic.Int32

	// rejectSessionOnce, when set, makes the next data request answer 401 (stale
	// session) exactly once, then serve normally — the re-auth trigger.
	rejectSessionOnce atomic.Bool

	// dataStatus overrides the status the data endpoints return (0 = 200 OK). Used to
	// exercise the typed-error classification.
	dataStatus atomic.Int32

	// loginStatus overrides the login response status (0 = 200 OK).
	loginStatus atomic.Int32
	// loginBody overrides the login response body (empty = "{}").
	loginBody atomic.Value // string
}

func (s *stubPortal) handler() http.Handler {
	mux := http.NewServeMux()

	// Login endpoint (the stub SSO).
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		s.logins.Add(1)
		if code := s.loginStatus.Load(); code != 0 {
			w.WriteHeader(int(code))
		}
		body := "{}"
		if v, ok := s.loginBody.Load().(string); ok && v != "" {
			body = v
		}
		// Set a session cookie so the jar carries it forward.
		http.SetCookie(w, &http.Cookie{Name: "eproc_session", Value: "abc"})
		_, _ = io.WriteString(w, body)
	})

	// Data endpoints.
	dataFunc := func(w http.ResponseWriter, r *http.Request, payload string) {
		if s.rejectSessionOnce.CompareAndSwap(true, false) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if code := s.dataStatus.Load(); code != 0 {
			w.WriteHeader(int(code))
			return
		}
		s.dataHits.Add(1)
		_, _ = io.WriteString(w, payload)
	}

	mux.HandleFunc("/api/processo/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/eventos") {
			dataFunc(w, r, `[{"id":"e1","data":"2026-01-02T10:00:00Z","descricao":"Distribuído","documentos":[{"id":"d1","rotulo":"Inicial","mime_type":"application/pdf"}]}]`)
			return
		}
		dataFunc(w, r, `{"numero_cnj":"1234567-89.2026.8.26.0100","classe":"Cumprimento de Sentença","orgao_julgador":"1ª Vara JEC Franca","partes":[{"nome":"Fulano","polo":"ativo","documento":"123.456.789-00"}]}`)
	})

	mux.HandleFunc("/api/documento/", func(w http.ResponseWriter, r *http.Request) {
		dataFunc(w, r, "%PDF-1.4 fake bytes")
	})

	return mux
}

// rewriteTransport redirects the absolute SSO login URL to the test server, so the
// client's real login flow runs against the stub without changing production code.
type rewriteTransport struct {
	base   http.RoundTripper
	server string // test server base, e.g. http://127.0.0.1:PORT
}

func (t *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.Contains(req.URL.String(), "sso.tjsp.jus.br") {
		u, _ := req.URL.Parse(t.server + "/login")
		req = req.Clone(req.Context())
		req.URL = u
		req.Host = u.Host
	}
	return t.base.RoundTrip(req)
}

// newTestClient wires a Client to the stub: base URL points at the server, and the
// transport rewrites the SSO login URL to the same server's /login.
func newTestClient(t *testing.T, srv *httptest.Server, creds Credentials) *Client {
	t.Helper()
	hc := &http.Client{
		Transport: &rewriteTransport{base: http.DefaultTransport, server: srv.URL},
	}
	return NewEprocClient(hc,
		WithBaseURL(srv.URL),
		WithCredentials(creds),
	)
}

func validCreds() Credentials {
	return Credentials{Username: "advogado@example.com", Password: "s3nh4"}
}

// TestClient_SessionReusedAcrossCalls proves the session is established once and reused:
// three read calls in a row trigger exactly ONE login.
func TestClient_SessionReusedAcrossCalls(t *testing.T) {
	t.Parallel()

	stub := &stubPortal{}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	c := newTestClient(t, srv, validCreds())
	ctx := context.Background()

	if _, err := c.GetProcess(ctx, "1234567-89.2026.8.26.0100"); err != nil {
		t.Fatalf("GetProcess #1: %v", err)
	}
	if _, err := c.ListEvents(ctx, "1234567-89.2026.8.26.0100"); err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if _, err := c.GetProcess(ctx, "1234567-89.2026.8.26.0100"); err != nil {
		t.Fatalf("GetProcess #2: %v", err)
	}

	if got := stub.logins.Load(); got != 1 {
		t.Errorf("logins = %d, want 1 (session must be reused across calls)", got)
	}
	if c.Status() != StatusConnected {
		t.Errorf("status = %q, want CONNECTED", c.Status())
	}
}

// TestClient_ReAuthOn401 proves a stale session (401) triggers exactly one re-auth and
// the call then succeeds — the automatic re-auth requirement.
func TestClient_ReAuthOn401(t *testing.T) {
	t.Parallel()

	stub := &stubPortal{}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	c := newTestClient(t, srv, validCreds())
	ctx := context.Background()

	// First call logs in (login #1) and succeeds.
	if _, err := c.GetProcess(ctx, "x"); err != nil {
		t.Fatalf("GetProcess #1: %v", err)
	}
	// Arrange: the next data request will 401 once, forcing a re-auth (login #2).
	stub.rejectSessionOnce.Store(true)

	if _, err := c.GetProcess(ctx, "x"); err != nil {
		t.Fatalf("GetProcess after stale session: %v", err)
	}

	if got := stub.logins.Load(); got != 2 {
		t.Errorf("logins = %d, want 2 (one initial + one re-auth)", got)
	}
}

// TestClient_ReAuthGivesUpAfterSecondRejection proves we do NOT loop: if the session is
// still rejected after re-authenticating, the error is a typed Unauthorized.
func TestClient_ReAuthGivesUpAfterSecondRejection(t *testing.T) {
	t.Parallel()

	stub := &stubPortal{}
	// Every data request 401s: dataStatus 401 makes classifyStatus/rejection persistent.
	stub.dataStatus.Store(http.StatusUnauthorized)
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	c := newTestClient(t, srv, validCreds())

	_, err := c.GetProcess(context.Background(), "x")
	if !IsUnauthorized(err) {
		t.Fatalf("err = %v, want Unauthorized after persistent rejection", err)
	}
}

// TestClient_TypedErrors proves the read maps portal statuses to distinct typed errors:
// forbidden ≠ not-found ≠ unavailable.
func TestClient_TypedErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		dataStatus int
		check      func(error) bool
		checkName  string
	}{
		{name: "access denied", dataStatus: http.StatusForbidden, check: IsForbidden, checkName: "IsForbidden"},
		{name: "not found", dataStatus: http.StatusNotFound, check: IsNotFound, checkName: "IsNotFound"},
		{name: "portal error", dataStatus: http.StatusBadGateway, check: IsUnavailable, checkName: "IsUnavailable"},
		{name: "rate limited", dataStatus: http.StatusTooManyRequests, check: IsUnavailable, checkName: "IsUnavailable"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			stub := &stubPortal{}
			stub.dataStatus.Store(int32(tt.dataStatus))
			srv := httptest.NewServer(stub.handler())
			defer srv.Close()

			c := newTestClient(t, srv, validCreds())

			_, err := c.GetProcess(context.Background(), "x")
			if err == nil {
				t.Fatalf("GetProcess = nil error, want a typed error")
			}
			if !tt.check(err) {
				t.Errorf("err = %v, want %s to be true", err, tt.checkName)
			}
		})
	}
}

// TestClient_LoginFailedIsUnauthorized proves a rejected login is a typed Unauthorized,
// distinct from a network error.
func TestClient_LoginFailedIsUnauthorized(t *testing.T) {
	t.Parallel()

	stub := &stubPortal{}
	stub.loginStatus.Store(http.StatusUnauthorized)
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	c := newTestClient(t, srv, validCreds())

	err := c.Login(context.Background())
	if !IsUnauthorized(err) {
		t.Fatalf("Login err = %v, want Unauthorized", err)
	}
}

// TestClient_CaptchaDetected proves a captcha/MFA challenge in the login body is surfaced
// distinctly (Forbidden), not confused with a bad password — the D0 UNKNOWN #2.
func TestClient_CaptchaDetected(t *testing.T) {
	t.Parallel()

	stub := &stubPortal{}
	stub.loginBody.Store(`<html><body>Resolva o reCAPTCHA para continuar</body></html>`)
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	c := newTestClient(t, srv, validCreds())

	err := c.Login(context.Background())
	if !IsForbidden(err) {
		t.Fatalf("Login err = %v, want Forbidden (captcha/MFA challenge)", err)
	}
}

// TestClient_MissingCredentialsIsInvalid proves an empty credential is rejected before
// any network call.
func TestClient_MissingCredentialsIsInvalid(t *testing.T) {
	t.Parallel()

	c := NewEprocClient(nil, WithBaseURL("http://unused.invalid"))

	err := c.Login(context.Background())
	if err == nil {
		t.Fatal("Login with no credentials = nil, want an error")
	}
	if !kindIs(err, "DOMAIN_ERROR_INVALID") {
		t.Errorf("err = %v, want Invalid kind", err)
	}
}

// certisignRewriteTransport redirects the absolute Certisign URL to the test server, so
// the client's real certLogin flow runs against the stub without changing production
// code — mirroring rewriteTransport's role for the SSO login URL above.
type certisignRewriteTransport struct {
	base   http.RoundTripper
	server string
}

func (t *certisignRewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.Contains(req.URL.Host, "certisign.com.br") {
		u, _ := req.URL.Parse(t.server + "/certisign-login")
		req = req.Clone(req.Context())
		req.URL = u
		req.Host = u.Host
	}
	return t.base.RoundTrip(req)
}

func newCertTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	hc := &http.Client{
		Transport: &certisignRewriteTransport{base: http.DefaultTransport, server: srv.URL},
	}
	return NewEprocClient(hc, WithBaseURL(srv.URL), WithCertificateAuth())
}

// certisignAcceptedStub serves the auto-submitting form Certisign returns when it
// accepts the certificate presented during the (real) mTLS handshake — the "cb" token
// certLogin extracts and redeems at the callback path.
func certisignAcceptedStub(w http.ResponseWriter, r *http.Request) {
	_, _ = io.WriteString(w, `<html><body onload="document.getElementById('f').submit()">
<form id="f" method="post" action="https://eproc1g.tjsp.jus.br/eproc/externo_controlador.php?acao=entrar_certificado_certisign">
<input type="hidden" name="cb" value="opaque-test-token"/>
</form></body></html>`)
}

// certisignRejectedStub serves a body with NO "cb" field — what Certisign returns when
// the certificate is not recognized (no auto-submit form appears at all).
func certisignRejectedStub(w http.ResponseWriter, r *http.Request) {
	_, _ = io.WriteString(w, `<html><body>certificado não reconhecido</body></html>`)
}

// eprocCallbackStub serves the eproc side of certisignCallbackPath with a configurable
// status/body — the second hop, after Certisign has already vouched for the cert.
func eprocCallbackStub(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if status != 0 {
			w.WriteHeader(status)
		}
		_, _ = io.WriteString(w, body)
	}
}

// TestClient_CertLogin_Success proves the full two-hop flow: Certisign accepts the
// certificate (served the "cb" token), certLogin redeems it at eproc's callback, and a
// 2xx with no challenge/redirect marker establishes the session — no username/password
// ever leaves the client (there is no SSO endpoint registered in this stub at all).
func TestClient_CertLogin_Success(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/certisign-login", certisignAcceptedStub)
	mux.HandleFunc("/eproc/externo_controlador.php", eprocCallbackStub(0, "<html>bem-vindo</html>"))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newCertTestClient(t, srv)

	if err := c.Login(context.Background()); err != nil {
		t.Fatalf("Login = %v, want nil", err)
	}
	if c.Status() != StatusConnected {
		t.Fatalf("Status = %v, want CONNECTED", c.Status())
	}
}

// TestClient_CertLogin_MFARequired proves the expected TOTP step (documented in TJSP's
// own eproc manuals as mandatory after certificate recognition) surfaces as Forbidden,
// distinct from a rejected certificate — Certisign still accepted the cert; the
// challenge appears on the eproc side of the callback.
func TestClient_CertLogin_MFARequired(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/certisign-login", certisignAcceptedStub)
	mux.HandleFunc("/eproc/externo_controlador.php", eprocCallbackStub(0, `<html>Digite o código de verificação</html>`))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newCertTestClient(t, srv)

	err := c.Login(context.Background())
	if !IsForbidden(err) {
		t.Fatalf("Login err = %v, want Forbidden (MFA/TOTP required)", err)
	}
}

// TestClient_CertLogin_RejectedByCertisign proves Certisign never issuing a "cb" token
// (the certificate itself was not recognized/is expired) is Unauthorized — the eproc
// callback is never even called.
func TestClient_CertLogin_RejectedByCertisign(t *testing.T) {
	t.Parallel()

	calledEproc := false
	mux := http.NewServeMux()
	mux.HandleFunc("/certisign-login", certisignRejectedStub)
	mux.HandleFunc("/eproc/externo_controlador.php", func(w http.ResponseWriter, r *http.Request) {
		calledEproc = true
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newCertTestClient(t, srv)

	err := c.Login(context.Background())
	if !IsUnauthorized(err) {
		t.Fatalf("Login err = %v, want Unauthorized", err)
	}
	if calledEproc {
		t.Error("eproc callback was called despite Certisign never issuing a cb token")
	}
}

// TestClient_CertLogin_RejectedByEprocCallback proves a 401 from eproc's callback
// (Certisign vouched for the cert, but eproc itself rejects the token) is Unauthorized
// too, distinctly from a Certisign-side rejection.
func TestClient_CertLogin_RejectedByEprocCallback(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/certisign-login", certisignAcceptedStub)
	mux.HandleFunc("/eproc/externo_controlador.php", eprocCallbackStub(http.StatusUnauthorized, "<html>token inválido</html>"))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newCertTestClient(t, srv)

	err := c.Login(context.Background())
	if !IsUnauthorized(err) {
		t.Fatalf("Login err = %v, want Unauthorized", err)
	}
}

// TestClient_DownloadStreamsToWriter proves the download writes the document bytes to the
// provided writer (streamed) and reports the byte count.
func TestClient_DownloadStreamsToWriter(t *testing.T) {
	t.Parallel()

	stub := &stubPortal{}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	c := newTestClient(t, srv, validCreds())

	var buf bytes.Buffer
	n, err := c.DownloadDocument(context.Background(), "d1", &buf)
	if err != nil {
		t.Fatalf("DownloadDocument: %v", err)
	}
	if n != int64(buf.Len()) {
		t.Errorf("reported %d bytes, buffer has %d", n, buf.Len())
	}
	if !strings.HasPrefix(buf.String(), "%PDF") {
		t.Errorf("body = %q, want a PDF prefix", buf.String())
	}
}

// TestParseProcess_CarriesRawDocumentVerbatim proves the CPF/CNPJ field is carried
// exactly as received — the spike must not normalize away what Portão B needs to observe.
func TestParseProcess_CarriesRawDocumentVerbatim(t *testing.T) {
	t.Parallel()

	raw := `{"numero_cnj":"1-1.1.1.1.1","classe":"c","orgao_julgador":"o","partes":[{"nome":"n","polo":"ativo","documento":"123.***.***-00"}]}`
	proc, err := parseProcess(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("parseProcess: %v", err)
	}
	if len(proc.Parties) != 1 {
		t.Fatalf("parties = %d, want 1", len(proc.Parties))
	}
	if got := proc.Parties[0].RawDocument; got != "123.***.***-00" {
		t.Errorf("RawDocument = %q, want the verbatim masked value", got)
	}
}

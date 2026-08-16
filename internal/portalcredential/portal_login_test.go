package portalcredential

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// --- fixtures captured LIVE from the real TJSP eproc portal in this session --
// (curl against https://eproc1g.tjsp.jus.br/eproc/, one clean GET and one POST
// with a deliberately wrong password). Kept as excerpts, not full pages, but
// byte-for-byte from what the portal actually returned — see portal_login.go's
// header comment.

// loginFormFixture is an excerpt of the Keycloak login page reached after
// following externo_controlador.php?acao=SSO/login.
const loginFormFixture = `
<html><body>
<div class="login-pf-page">
<form id="kc-form-login" action="https://sso.tjsp.jus.br/realms/eproc/login-actions/authenticate?session_code=W1-K8N2be6VsXPcFJqKY_OqcGKMJh70iYD1qOwqpGKA&amp;execution=ad942024-79f0-4a9d-af4e-f0819167b569&amp;client_id=eproc1g.tjsp.jus.br&amp;tab_id=xMN518HPHoA&amp;client_data=eyJydSI6Imh0dHBzOi8vZXByb2MxZy50anNwLmp1cy5ici9lcHJvYy9leHRlcm5vX2NvbnRyb2xhZG9yLnBocD9hY2FvPVNTTy9jYWxsYmFjayIsInJ0IjoiY29kZSIsInN0IjoiNThmMjBjZDJlNGY0MDBmYTAzZjI0ODVkNmM0Zjg5YTUifQ&amp;eproc_client_id=eproc1g.tjsp.jus.br" method="post" autocomplete="off">
<input tabindex="2" id="username" class="form-control form-control-sm" name="username" value="" type="text" autofocus autocomplete="false" aria-autocomplete="off">
<input tabindex="3" id="password" class="form-control form-control-sm" name="password" type="password" autocomplete="new-password" aria-autocomplete="off">
</form>
</div>
</body></html>
`

// loginRejectedFixture is an excerpt of the SAME page re-rendered after a real
// POST with a wrong password — note the id="input-error" span with the exact
// Portuguese message the portal returned.
const loginRejectedFixture = `
<html><body>
<div class="login-pf-page">
<form id="kc-form-login" action="https://sso.tjsp.jus.br/realms/eproc/login-actions/authenticate?session_code=W1-K8N2be6VsXPcFJqKY_OqcGKMJh70iYD1qOwqpGKA&amp;execution=ad942024-79f0-4a9d-af4e-f0819167b569" method="post" autocomplete="off">
<input tabindex="2" id="username" class="form-control form-control-sm" name="username" value="teste.invalido.dev12345" type="text" autofocus autocomplete="false" aria-autocomplete="off" aria-invalid="true">
<span id="input-error" class="text-danger text-sm" aria-live="polite">
    Nome de usuário ou senha inválida.
</span>
<input tabindex="3" id="password" class="form-control form-control-sm" name="password" type="password" autocomplete="new-password" aria-autocomplete="off" aria-invalid="true">
</form>
</div>
</body></html>
`

func TestKCFormActionRe_ExtractsActionFromLiveFixture(t *testing.T) {
	t.Parallel()

	match := kcFormActionRe.FindStringSubmatch(loginFormFixture)
	if match == nil {
		t.Fatal("kcFormActionRe found no match against the live-captured login form")
	}
	if !strings.Contains(match[1], "sso.tjsp.jus.br/realms/eproc/login-actions/authenticate") {
		t.Errorf("extracted action = %q, want it to point at the Keycloak authenticate endpoint", match[1])
	}
	if !strings.Contains(match[1], "session_code=") || !strings.Contains(match[1], "execution=") {
		t.Errorf("extracted action = %q, want the session_code/execution binding params", match[1])
	}
}

func TestKCFormActionRe_NoMatchOnUnrelatedPage(t *testing.T) {
	t.Parallel()

	if match := kcFormActionRe.FindStringSubmatch("<html><body>not a login page</body></html>"); match != nil {
		t.Errorf("kcFormActionRe matched an unrelated page: %v", match)
	}
}

func TestKCLoginErrorRe_MatchesLiveRejectionFixture(t *testing.T) {
	t.Parallel()

	if !kcLoginErrorRe.MatchString(loginRejectedFixture) {
		t.Error("kcLoginErrorRe did not match the live-captured rejection fixture")
	}
	if kcLoginErrorRe.MatchString(loginFormFixture) {
		t.Error("kcLoginErrorRe matched the clean (non-rejected) login form fixture")
	}
}

func TestClassifyLoginResponse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		landedHost  string
		body        string
		wantOutcome LoginOutcome
	}{
		{
			name:        "left sso domain — success",
			landedHost:  "eproc1g.tjsp.jus.br",
			body:        "<html>bem-vindo</html>",
			wantOutcome: LoginOutcomeSuccess,
		},
		{
			name:        "stayed on sso with rejection markup — rejected",
			landedHost:  "sso.tjsp.jus.br",
			body:        loginRejectedFixture,
			wantOutcome: LoginOutcomeRejected,
		},
		{
			name:        "stayed on sso with captcha marker — inconclusive",
			landedHost:  "sso.tjsp.jus.br",
			body:        `<div class="g-recaptcha" data-sitekey="..."></div>`,
			wantOutcome: LoginOutcomeInconclusive,
		},
		{
			name:        "stayed on sso, unrecognized body — inconclusive",
			landedHost:  "sso.tjsp.jus.br",
			body:        "<html>algo inesperado</html>",
			wantOutcome: LoginOutcomeInconclusive,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := classifyLoginResponse(tt.landedHost, []byte(tt.body))
			if got.Outcome != tt.wantOutcome {
				t.Errorf("classifyLoginResponse() outcome = %v, want %v (detail=%q)", got.Outcome, tt.wantOutcome, got.Detail)
			}
		})
	}
}

// --- end-to-end Check() against a fake Keycloak-shaped server ---------------
//
// Check() always targets the real tjspEprocLoginStartURL host, so this harness
// redirects every dial to a local httptest.Server regardless of the requested
// host (the URL string — and therefore resp.Request.URL.Host, what
// classifyLoginResponse reads — is untouched; only the TCP connection is
// rerouted). This exercises the FULL live code path (GET, regex-extract the
// action, POST, follow redirects, classify) without any real network call.

// newRedirectingClient builds an *http.Client whose Transport dials srv for
// every request, whatever host/port the request URL names.
func newRedirectingClient(t *testing.T, srv *httptest.Server) *http.Client {
	t.Helper()

	dialer := &net.Dialer{Timeout: 5 * time.Second}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, srv.Listener.Addr().String())
		},
		DialTLSContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			rawConn, err := dialer.DialContext(ctx, network, srv.Listener.Addr().String())
			if err != nil {
				return nil, err
			}
			return tls.Client(rawConn, &tls.Config{InsecureSkipVerify: true}), nil //nolint:gosec // test-only fixture server
		},
	}
	return &http.Client{Transport: transport, Timeout: 5 * time.Second}
}

func TestTJSPEprocChecker_Check_Success(t *testing.T) {
	t.Parallel()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Write([]byte(loginFormFixture)) //nolint:errcheck
			return
		}
		// POST to the authenticate action: correct creds → redirect off the sso
		// domain, mirroring Keycloak's real post-auth OIDC redirect.
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm: %v", err)
		}
		if r.FormValue("username") == "advogado.real" && r.FormValue("password") == "senha-correta" {
			http.Redirect(w, r, "https://eproc1g.tjsp.jus.br/eproc/externo_controlador.php?acao=SSO/callback", http.StatusFound)
			return
		}
		w.Write([]byte(loginRejectedFixture)) //nolint:errcheck
	}))
	defer srv.Close()

	checker := NewTJSPEprocChecker(WithHTTPClient(newRedirectingClient(t, srv)))
	result, err := checker.Check(context.Background(), "advogado.real", "senha-correta")
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if result.Outcome != LoginOutcomeSuccess {
		t.Errorf("Check() outcome = %v, want Success (detail=%q)", result.Outcome, result.Detail)
	}
}

func TestTJSPEprocChecker_Check_Rejected(t *testing.T) {
	t.Parallel()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Write([]byte(loginFormFixture)) //nolint:errcheck
			return
		}
		w.Write([]byte(loginRejectedFixture)) //nolint:errcheck
	}))
	defer srv.Close()

	checker := NewTJSPEprocChecker(WithHTTPClient(newRedirectingClient(t, srv)))
	result, err := checker.Check(context.Background(), "advogado.real", "senha-errada")
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if result.Outcome != LoginOutcomeRejected {
		t.Errorf("Check() outcome = %v, want Rejected (detail=%q)", result.Outcome, result.Detail)
	}
}

func TestTJSPEprocChecker_Check_InconclusiveWhenFormMissing(t *testing.T) {
	t.Parallel()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<html>portal fora do ar / página desconhecida</html>")) //nolint:errcheck
	}))
	defer srv.Close()

	checker := NewTJSPEprocChecker(WithHTTPClient(newRedirectingClient(t, srv)))
	result, err := checker.Check(context.Background(), "advogado.real", "qualquer")
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if result.Outcome != LoginOutcomeInconclusive {
		t.Errorf("Check() outcome = %v, want Inconclusive (detail=%q)", result.Outcome, result.Detail)
	}
}

func TestTJSPEprocChecker_Check_InconclusiveOnTransportError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(loginFormFixture)) //nolint:errcheck
	}))
	srv.Close() // closed before any request — every dial fails

	checker := NewTJSPEprocChecker(WithHTTPClient(newRedirectingClient(t, srv)))
	result, err := checker.Check(context.Background(), "advogado.real", "qualquer")
	if err != nil {
		t.Fatalf("Check() error = %v, want nil (transport faults collapse into Inconclusive)", err)
	}
	if result.Outcome != LoginOutcomeInconclusive {
		t.Errorf("Check() outcome = %v, want Inconclusive", result.Outcome)
	}
}

func TestTJSPEprocChecker_Check_PropagatesContextCancellation(t *testing.T) {
	t.Parallel()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(loginFormFixture)) //nolint:errcheck
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	checker := NewTJSPEprocChecker(WithHTTPClient(newRedirectingClient(t, srv)))
	if _, err := checker.Check(ctx, "advogado.real", "qualquer"); err == nil {
		t.Error("Check() error = nil, want the cancellation error to propagate")
	}
}

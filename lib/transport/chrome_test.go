package transport

import (
	"net/url"
	"testing"
)

// TestChromeTransport_Nil is a characterization test: it pins the shape the DJEN
// connector (and now eproc) rely on when no proxy is configured. The uTLS dialer must
// be wired into DialTLSContext, Transport.Proxy must stay nil (the CONNECT tunnel is
// opened inside DialTLSContext, not by net/http), and HTTP/2 must be off (the uTLS
// hello pins ALPN to http/1.1).
func TestChromeTransport_Nil(t *testing.T) {
	t.Parallel()

	tr := ChromeTransport(nil, nil)

	if tr == nil {
		t.Fatal("ChromeTransport(nil, nil) = nil, want a *http.Transport")
	}
	if tr.DialTLSContext == nil {
		t.Error("DialTLSContext = nil, want the uTLS Chrome dialer")
	}
	if tr.Proxy != nil {
		t.Error("Transport.Proxy set, want nil (CONNECT handled inside DialTLSContext)")
	}
	if tr.ForceAttemptHTTP2 {
		t.Error("ForceAttemptHTTP2 = true, want false (uTLS hello pins ALPN to http/1.1)")
	}
}

// TestChromeTransport_WithProxy asserts a proxy URL still yields the same shape — the
// tunnel is applied inside DialTLSContext, so Transport.Proxy stays nil regardless.
func TestChromeTransport_WithProxy(t *testing.T) {
	t.Parallel()

	proxyURL, err := url.Parse("http://user:pass@proxy.example:8080")
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}

	tr := ChromeTransport(proxyURL, nil)

	if tr.DialTLSContext == nil {
		t.Error("DialTLSContext = nil, want the uTLS Chrome dialer even with a proxy")
	}
	if tr.Proxy != nil {
		t.Error("Transport.Proxy set, want nil (proxy tunneled inside DialTLSContext)")
	}
}

// TestProxyHostPort checks the scheme-default port derivation kept alongside the moved
// proxy dialer.
func TestProxyHostPort(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "explicit port", raw: "http://proxy.example:3128", want: "proxy.example:3128"},
		{name: "http default port", raw: "http://proxy.example", want: "proxy.example:80"},
		{name: "https default port", raw: "https://proxy.example", want: "proxy.example:443"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			is := is(t)

			u, err := url.Parse(tt.raw)
			if err != nil {
				t.Fatalf("url.Parse(%q): %v", tt.raw, err)
			}
			is(proxyHostPort(u), tt.want)
		})
	}
}

// is returns a tiny equality helper bound to this subtest's t — avoids the
// parent-scope assert leak while keeping the table terse.
func is(t *testing.T) func(got, want string) {
	t.Helper()
	return func(got, want string) {
		t.Helper()
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

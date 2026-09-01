// Package transport provides a shared *http.Transport whose TLS layer presents a
// current Chrome fingerprint via uTLS, optionally tunneling through an HTTP CONNECT
// proxy.
//
// It exists because more than one connector needs the same anti-bot posture: the
// DJEN Comunica WAF rate-limits by JA3 (Go's default ClientHello is throttled where
// Chrome sails through), and the TJSP eproc portal sits behind similar edge
// protection. Keeping the handshake in ONE place makes that fingerprint a single
// source of truth instead of a per-connector copy.
//
// It is pure stdlib + uTLS on purpose — it MUST NOT import internal/* or any other
// lib domain package. Callers own the *http.Client; this package only builds the
// transport.
package transport

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	utls "github.com/refraction-networking/utls"
)

// chromeHello is the uTLS ClientHello preset the transport presents — a Chrome
// fingerprint, so a per-JA3 rate limiter grants a browser budget, not the bot-sized
// one Go's default handshake earns.
var chromeHello = utls.HelloChrome_120

// ChromeTransport builds an *http.Transport whose TLS layer presents the Chrome
// fingerprint via uTLS, optionally tunneling through an HTTP CONNECT proxy. The proxy
// is handled INSIDE DialTLSContext (not Transport.Proxy) because the uTLS handshake
// must run over the tunneled conn — the Proxy field would run the stdlib's own TLS
// instead and lose the fingerprint. A nil proxyURL keeps the direct connection.
//
// clientCert is optional (nil for every caller except the eproc cert-login spike):
// when set, it is presented during the handshake for mutual TLS — the mechanism
// eproc's "Certificado Digital" login uses (the browser's native certificate picker
// is exactly what a server-requested client cert triggers). The stdlib
// *tls.Certificate shape keeps this package's only non-stdlib dependency (uTLS)
// from leaking into callers.
func ChromeTransport(proxyURL *url.URL, clientCert *tls.Certificate) *http.Transport {
	return &http.Transport{
		ForceAttemptHTTP2: false, // the uTLS hello pins ALPN to http/1.1
		MaxIdleConns:      10,
		IdleConnTimeout:   90 * time.Second,
		DialTLSContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			return dialChromeTLS(ctx, proxyURL, clientCert, addr)
		},
	}
}

// dialChromeTLS opens the TCP conn (direct or via the proxy's CONNECT tunnel) and
// completes a uTLS handshake presenting the Chrome ClientHello, with ALPN pinned to
// http/1.1 so net/http speaks HTTP/1.1 over it (the cipher/extension fingerprint the
// WAF keys on is preserved). clientCert, when non-nil, is offered if the peer
// requests client authentication (mutual TLS).
func dialChromeTLS(ctx context.Context, proxyURL *url.URL, clientCert *tls.Certificate, addr string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	var raw net.Conn
	var err error
	if proxyURL != nil {
		raw, err = proxyConnect(ctx, dialer, proxyURL, addr)
	} else {
		raw, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, err
	}

	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		_ = raw.Close()
		return nil, err
	}
	spec, err := utls.UTLSIdToSpec(chromeHello)
	if err != nil {
		_ = raw.Close()
		return nil, err
	}
	for _, ext := range spec.Extensions {
		if a, ok := ext.(*utls.ALPNExtension); ok {
			a.AlpnProtocols = []string{"http/1.1"}
		}
	}
	tlsConfig := &utls.Config{ServerName: host}
	if clientCert != nil {
		tlsConfig.Certificates = []utls.Certificate{{
			Certificate: clientCert.Certificate,
			PrivateKey:  clientCert.PrivateKey,
		}}
	}
	u := utls.UClient(raw, tlsConfig, utls.HelloCustom)
	if err := u.ApplyPreset(&spec); err != nil {
		_ = raw.Close()
		return nil, err
	}
	if err := u.HandshakeContext(ctx); err != nil {
		_ = raw.Close()
		return nil, err
	}
	return u, nil
}

// proxyConnect dials the proxy and opens an HTTP CONNECT tunnel to target, returning
// the tunneled conn for the caller to run TLS over. Basic auth from the proxy URL
// userinfo is sent when present.
func proxyConnect(ctx context.Context, dialer *net.Dialer, proxyURL *url.URL, target string) (net.Conn, error) {
	conn, err := dialer.DialContext(ctx, "tcp", proxyHostPort(proxyURL))
	if err != nil {
		return nil, fmt.Errorf("dial proxy: %w", err)
	}
	req := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n", target, target)
	if proxyURL.User != nil {
		user := proxyURL.User.Username()
		pass, _ := proxyURL.User.Password()
		cred := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
		req += "Proxy-Authorization: Basic " + cred + "\r\n"
	}
	req += "\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("proxy CONNECT write: %w", err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("proxy CONNECT read: %w", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_ = conn.Close()
		return nil, fmt.Errorf("proxy CONNECT %s: status %s", target, resp.Status)
	}
	return conn, nil
}

// proxyHostPort returns the proxy's host:port, defaulting the port by scheme when
// omitted.
func proxyHostPort(u *url.URL) string {
	if u.Port() != "" {
		return u.Host
	}
	if u.Scheme == "https" {
		return u.Hostname() + ":443"
	}
	return u.Hostname() + ":80"
}

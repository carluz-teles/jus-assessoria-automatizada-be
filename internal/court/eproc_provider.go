package court

import (
	"context"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/url"
	"time"

	"github.com/jusassessoria/platform/lib/eproc"
	"github.com/jusassessoria/platform/lib/transport"
)

// CertSigner is the narrow port EprocProvider uses to get a KMS-backed crypto.Signer
// for a lawyer's certificate — implemented by internal/certificate.UseCase.NewSigner,
// wired via a small adapter in cmd/api/main.go (the same structural-typing pattern
// internal/draft already uses for its own CertSigner — see draft.CertSigner /
// certSignerFunc). court never imports internal/certificate directly: slices only
// talk through a port + adapter, never entity-to-entity.
type CertSigner interface {
	NewSigner(ctx context.Context, tenantID, certificateID string) (crypto.Signer, *x509.Certificate, []*x509.Certificate, error)
}

// EprocProvider implements CourtProvider for TJSP's eproc, wrapping lib/eproc — the
// client that CONFIRMED, live against the real portal, that certificate-only login
// works via Keycloak's native X.509 authenticator (no browser extension, no Desktop
// Agent). This provider's only job is turning a CourtConnection (vault pointers) into
// the concrete inputs lib/eproc needs (a client cert, a TOTP seed) and mapping its
// errors back onto the court state machine.
//
// It deliberately does NOT implement MFAEnroller: investigation (2026-08-29) found
// eproc's own MFA reconfiguration flow requires the lawyer's username/password (a
// hidden `2fa_configurar` action on the login page's own JS, or an equivalent
// authenticated self-service page) — a certificate-authenticated session cannot
// trigger it. Every real-world alternative in this market (including the competitor
// the client already compared us to) has the SAME limitation and solves it the same
// way: the lawyer captures their existing/reconfigured QR ONCE, by hand, and the
// platform takes it from there forever after (see UseCase.SubmitMFASeed). Automating
// the one-time capture itself remains open for a FUTURE portal/adapter that turns out
// to support it — MFAEnroller stays in provider.go for exactly that case.
type EprocProvider struct {
	certSigner CertSigner
	proxyURL   *url.URL // optional residential/BR proxy — same anti-bot posture DJEN needed
}

// EprocProviderOption tunes an EprocProvider at construction.
type EprocProviderOption func(*EprocProvider)

// WithEprocProxy sets the outbound proxy (nil keeps a direct connection).
func WithEprocProxy(proxyURL *url.URL) EprocProviderOption {
	return func(p *EprocProvider) { p.proxyURL = proxyURL }
}

// NewEprocProvider builds the eproc adapter. certSigner is required — a connection
// with AuthenticationMethod other than CERTIFICATE_A1 is out of scope for now (eproc's
// only CONFIRMED login path is certificate-based; password mode in lib/eproc remains
// ASSUMPTION/Portão B, per its own doc).
func NewEprocProvider(certSigner CertSigner, opts ...EprocProviderOption) *EprocProvider {
	p := &EprocProvider{certSigner: certSigner}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Connect authenticates conn's lawyer against eproc using their certificate, and —
// when seed is non-empty — completes the mandatory TOTP step automatically
// (eproc.WithTOTPSeed). Returns ErrMFAEnrollmentRequired when the portal challenges
// for a second factor and seed is "" (Connect never attempts a code without one).
func (p *EprocProvider) Connect(ctx context.Context, conn *CourtConnection, seed string) error {
	client, err := p.buildClient(ctx, conn, seed)
	if err != nil {
		return err
	}

	err = client.Login(ctx)
	switch {
	case err == nil:
		return nil
	case eproc.IsForbidden(err) && seed == "":
		// classifyLoginResult's exact signal for "certificate accepted, MFA/TOTP
		// challenge required" (lib/eproc/auth.go) with no seed configured to try —
		// the use case reacts by running EnrollMFA, never by retrying Connect blind.
		return ErrMFAEnrollmentRequired
	default:
		return err
	}
}

// buildClient resolves conn's certificate into a *tls.Certificate (leaf + chain,
// KMS-backed signer — the private key never leaves internal/certificate's vault
// boundary, only Sign operations do) and wires a lib/eproc client around it.
func (p *EprocProvider) buildClient(ctx context.Context, conn *CourtConnection, seed string) (*eproc.Client, error) {
	signer, leaf, intermediates, err := p.certSigner.NewSigner(ctx, conn.TenantID, conn.CertificateRef)
	if err != nil {
		return nil, err
	}

	chain := make([][]byte, 0, 1+len(intermediates))
	chain = append(chain, leaf.Raw)
	for _, ic := range intermediates {
		chain = append(chain, ic.Raw)
	}
	clientCert := &tls.Certificate{Certificate: chain, PrivateKey: signer, Leaf: leaf}

	hc := &http.Client{
		Timeout:   90 * time.Second,
		Transport: transport.ChromeTransport(p.proxyURL, clientCert),
	}
	opts := []eproc.Option{eproc.WithCertificateAuth()}
	if seed != "" {
		opts = append(opts, eproc.WithTOTPSeed(seed))
	}
	return eproc.NewEprocClient(hc, opts...), nil
}

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

// EprocProvider implements CourtProvider (and MFAEnroller) for TJSP's eproc, wrapping
// lib/eproc — the client that CONFIRMED, live against the real portal, that
// certificate-only login works via Keycloak's native X.509 authenticator (no browser
// extension, no Desktop Agent). This provider's only job is turning a CourtConnection
// (vault pointers) into the concrete inputs lib/eproc needs (a client cert, a TOTP
// seed) and mapping its errors back onto the court state machine.
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

// EnrollMFA is intentionally NOT implemented yet — this is the next Portão B
// investigation (finding eproc's real "habilitar autenticação em dois fatores"
// endpoint and confirming whether it renders a fresh QR/manual-key setup or an
// existing-credential challenge for an account that already has TOTP configured
// elsewhere — see the roadmap plan's §4). Returning a typed, clearly-labeled error
// keeps the rest of the slice (schema, Connect, the automatic-retry orchestration in
// domain.go, TOTP code generation/submission) shippable and tested independently of
// this one open unknown.
func (p *EprocProvider) EnrollMFA(ctx context.Context, conn *CourtConnection) (string, error) {
	return "", ErrMFAEnrollmentFailed
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

package court

import (
	"bytes"
	"context"
	"crypto"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
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
	docWriter  DocumentWriter // nil is fine — FetchAutos just skips document download (S3 not configured, same optional-adapter posture as the rest of the stack)
	proxyURL   *url.URL       // optional residential/BR proxy — same anti-bot posture DJEN needed
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
// ASSUMPTION/Portão B, per its own doc). docWriter may be nil (document download
// disabled — FetchAutos still fetches process/docket metadata normally).
func NewEprocProvider(certSigner CertSigner, docWriter DocumentWriter, opts ...EprocProviderOption) *EprocProvider {
	p := &EprocProvider{certSigner: certSigner, docWriter: docWriter}
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
	client, err := p.buildClient(ctx, conn, seed, nil)
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

// FetchAutos primes a fresh eproc.Client from sessionIn (buildClient's WithSession),
// pulls the process + its docket, and ALWAYS exports the resulting session before
// returning — even on a mid-call failure, since doAuthed may have silently
// re-authenticated before the failing request. A genuinely broken credential
// surfaces as apperr Unauthorized/Forbidden (see doAuthed's doc: a re-auth that
// itself fails is not retried, it's surfaced) — anything else is a transient,
// per-item fault the use case retries by requeuing.
func (p *EprocProvider) FetchAutos(ctx context.Context, conn *CourtConnection, seed string, sessionIn Session, courtRecordID, cnjNumber string, docketCursor time.Time) (AutosResult, Session, error) {
	client, err := p.buildClient(ctx, conn, seed, sessionIn)
	if err != nil {
		return AutosResult{}, sessionIn, err
	}

	proc, procErr := client.GetProcess(ctx, cnjNumber)
	var events []eproc.Event
	var eventsErr error
	downloaded := 0
	if procErr == nil {
		events, eventsErr = client.ListEvents(ctx, cnjNumber)
	}
	if eventsErr == nil {
		downloaded = p.downloadNewDocuments(ctx, client, conn.TenantID, courtRecordID, events, docketCursor)
	}

	sessionOut, exportErr := client.ExportSession()
	if exportErr != nil {
		sessionOut = sessionIn // exporting itself failed (rare) — don't lose what we already had
	}

	if procErr != nil {
		return AutosResult{}, sessionOut, procErr
	}
	if eventsErr != nil {
		return AutosResult{}, sessionOut, eventsErr
	}
	result := newAutosResult(proc, events)
	result.DocumentsDownloaded = downloaded
	return result, sessionOut, nil
}

// downloadNewDocuments downloads every document attached to an event STRICTLY
// newer than docketCursor (the incremental cut FetchAutos's own doc explains —
// events older than the cursor were already processed on a previous call, so
// their documents were already downloaded then) and hands each to p.docWriter.
// A single document's failure (download error, DocumentWriter error) is
// skipped, never aborts the batch: the docket/process metadata already
// succeeded by the time this runs, and the OTHER documents here are
// independent — losing one must not lose all. p.docWriter == nil (no S3
// configured) is a silent no-op, matching the rest of the stack's
// optional-adapter posture.
func (p *EprocProvider) downloadNewDocuments(ctx context.Context, client *eproc.Client, tenantID, courtRecordID string, events []eproc.Event, docketCursor time.Time) int {
	if p.docWriter == nil {
		return 0
	}
	downloaded := 0
	for _, ev := range events {
		if !ev.Date.After(docketCursor) {
			continue
		}
		for _, doc := range ev.Documents {
			if p.downloadOneDocument(ctx, client, tenantID, courtRecordID, doc) {
				downloaded++
			}
		}
	}
	return downloaded
}

func (p *EprocProvider) downloadOneDocument(ctx context.Context, client *eproc.Client, tenantID, courtRecordID string, doc eproc.DocumentRef) bool {
	var buf bytes.Buffer
	if _, err := client.DownloadDocument(ctx, doc.DownloadPath, &buf); err != nil {
		return false
	}
	data := buf.Bytes()
	sum := sha256.Sum256(data)
	checksum := hex.EncodeToString(sum[:])
	_, err := p.docWriter.WriteDocument(ctx, tenantID, courtRecordID, doc.MIMEType, checksum, doc.Label, data)
	return err == nil
}

func newAutosResult(proc *eproc.Process, events []eproc.Event) AutosResult {
	var latest time.Time
	for _, e := range events {
		if e.Date.After(latest) {
			latest = e.Date
		}
	}
	return AutosResult{Process: *proc, Events: events, LatestCursor: latest}
}

// buildClient resolves conn's certificate into a *tls.Certificate (leaf + chain,
// KMS-backed signer — the private key never leaves internal/certificate's vault
// boundary, only Sign operations do) and wires a lib/eproc client around it,
// primed with sessionIn when non-empty (nil is fine — WithSession no-ops on it).
func (p *EprocProvider) buildClient(ctx context.Context, conn *CourtConnection, seed string, sessionIn Session) (*eproc.Client, error) {
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
	opts := []eproc.Option{eproc.WithCertificateAuth(), eproc.WithSession(sessionIn)}
	if seed != "" {
		opts = append(opts, eproc.WithTOTPSeed(seed))
	}
	return eproc.NewEprocClient(hc, opts...), nil
}

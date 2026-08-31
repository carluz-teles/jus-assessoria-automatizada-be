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
	"strings"
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
	certSigner   CertSigner
	docWriter    DocumentWriter    // nil is fine — FetchAutos just skips document download (S3 not configured, same optional-adapter posture as the rest of the stack)
	recordWriter CourtRecordWriter // nil is fine — FetchAutos then skips the capa-metadata enrichment
	partyWriter  PartyWriter       // nil is fine — FetchAutos then skips persisting the capa parties
	proxyURL     *url.URL          // optional residential/BR proxy — same anti-bot posture DJEN needed
}

// EprocProviderOption tunes an EprocProvider at construction.
type EprocProviderOption func(*EprocProvider)

// WithEprocProxy sets the outbound proxy (nil keeps a direct connection).
func WithEprocProxy(proxyURL *url.URL) EprocProviderOption {
	return func(p *EprocProvider) { p.proxyURL = proxyURL }
}

// WithCourtRecordWriter injects the port that enriches the court_record with the
// capa metadata FetchAutos reads (nil keeps that a no-op).
func WithCourtRecordWriter(w CourtRecordWriter) EprocProviderOption {
	return func(p *EprocProvider) { p.recordWriter = w }
}

// WithPartyWriter injects the port that persists the capa parties (autor/réu + CPF/CNPJ
// + advogados) FetchAutos reads into the shared party/party_counsel tables (nil keeps
// that a no-op).
func WithPartyWriter(w PartyWriter) EprocProviderOption {
	return func(p *EprocProvider) { p.partyWriter = w }
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
	if procErr == nil {
		p.writeProcessMetadata(ctx, conn.TenantID, courtRecordID, proc)
		p.writeParties(ctx, conn.TenantID, courtRecordID, proc)
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
			if p.downloadOneDocument(ctx, client, tenantID, courtRecordID, doc, ev.Description) {
				downloaded++
			}
		}
	}
	return downloaded
}

func (p *EprocProvider) downloadOneDocument(ctx context.Context, client *eproc.Client, tenantID, courtRecordID string, doc eproc.DocumentRef, eventDescription string) bool {
	var buf bytes.Buffer
	if _, err := client.DownloadDocument(ctx, doc.DownloadPath, &buf); err != nil {
		return false
	}
	data := buf.Bytes()
	sum := sha256.Sum256(data)
	checksum := hex.EncodeToString(sum[:])
	title, documentType := docTitleAndType(doc.Label, eventDescription)
	_, err := p.docWriter.WriteDocument(ctx, tenantID, courtRecordID, doc.MIMEType, checksum, title, documentType, data)
	return err == nil
}

// docTitleAndType turns the eproc document's terse code (data-nome, e.g. "PET",
// "CONTRSOCIAL") into the pair the document row stores: a friendly pt-BR title and
// the raw code as document_type (categorization). The title prefers the code's
// mapped label; when the code is unknown it falls back to the event's description
// (infraEventoDescricao — "Petição inicial", "Despacho saneador"), and only then to
// the raw code, so a title is never blank.
func docTitleAndType(code, eventDescription string) (title, documentType string) {
	documentType = strings.TrimSpace(code)
	title = eproc.DocumentTypeLabel(documentType)
	if title == "" {
		title = strings.TrimSpace(eventDescription)
	}
	if title == "" {
		title = documentType
	}
	return title, documentType
}

// writeProcessMetadata hands the eproc capa metadata to the CourtRecordWriter to
// enrich the court_record. A nil writer (api process, or S3-less dev) is a no-op; a
// write error is swallowed on purpose — the metadata is a best-effort enrichment,
// never a reason to fail (and thus retry) the whole autos fetch, whose real payload
// (documents + docket cursor) already succeeded by the time this runs.
func (p *EprocProvider) writeProcessMetadata(ctx context.Context, tenantID, courtRecordID string, proc *eproc.Process) {
	if p.recordWriter == nil || proc == nil {
		return
	}
	_ = p.recordWriter.UpdateProcessMetadata(ctx, tenantID, courtRecordID, ProcessMetadata{
		Class:       proc.Class,
		JudgingBody: proc.JudgingBody,
		Magistrate:  proc.Magistrate,
		Situation:   proc.Situation,
		Competence:  proc.Competence,
		FiledAt:     proc.FiledAt,
	})
}

// writeParties hands the eproc capa parties (autor/réu + CPF/CNPJ + advogados) to the
// PartyWriter to persist into the shared party/party_counsel tables (source='EPROC',
// fill-if-missing — see PartyWriter's doc). A nil writer (api process, or a fetch with
// no party sink) is a no-op; a write error is swallowed on purpose — persisting the
// parties is a best-effort enrichment, never a reason to fail (and thus retry) the whole
// autos fetch, whose real payload (documents + docket cursor) already succeeded by the
// time this runs. Same posture as writeProcessMetadata.
func (p *EprocProvider) writeParties(ctx context.Context, tenantID, courtRecordID string, proc *eproc.Process) {
	if p.partyWriter == nil || proc == nil || len(proc.Parties) == 0 {
		return
	}
	parties := make([]ProcessParty, 0, len(proc.Parties))
	for _, pt := range proc.Parties {
		counsels := make([]ProcessCounsel, 0, len(pt.Counsels))
		for _, c := range pt.Counsels {
			counsels = append(counsels, ProcessCounsel{Name: c.Name, OAB: c.OAB, UF: c.UF})
		}
		parties = append(parties, ProcessParty{
			Role:     pt.Role,
			Name:     pt.Name,
			Document: pt.Document,
			Counsels: counsels,
		})
	}
	_ = p.partyWriter.UpsertParties(ctx, tenantID, courtRecordID, parties)
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

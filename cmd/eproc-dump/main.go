// Command eproc-dump is the D0 spike tool (docs/erd-execucao-judicial-tjsp.md §10.2).
// It is what a human runs the moment a real lawyer credential is available (Portão B):
// it authenticates against the TJSP eproc portal, reuses the session, lists one process
// + its events, downloads one document to disk, and prints the evidence the spike needs
// to resolve its two original UNKNOWNs —
//
//  1. does the portal expose CPF/CNPJ of parties (in clear, masked, or absent) without a
//     procuração? → we print the RAW document field of every party, verbatim.
//  2. does a captcha/MFA appear at login or in the autos? → we print whether the flow hit
//     a challenge.
//
// It reads everything from the environment; it NEVER logs a credential or private key
// (not even in debug). The Chrome uTLS transport (lib/transport) is built HERE and
// injected into the pure lib/eproc client — the lib stays free of the transport
// dependency, and stays free of internal/certificate too (it never sees key material,
// only the *http.Client the caller hands it — already mTLS-authenticated when cert mode
// is used).
//
// Two auth modes, mutually exclusive:
//
//   - Password: EPROC_USERNAME/EPROC_PASSWORD (the original D0 mode).
//   - Certificate (new UNKNOWN #3 — eproc's real mechanism for "Certificado Digital"
//     login, per TJSP's own manuals, is mutual TLS: the browser's native certificate
//     picker is what a server-requested client cert triggers): EPROC_CERT_TENANT_ID +
//     EPROC_CERT_ID identify a row already in this box's `certificate` table (KMS-backed
//     vault — see internal/certificate); the tool opens the vault, builds a *tls.
//     Certificate from the leaf+chain+crypto.Signer, and hands it to
//     transport.ChromeTransport so the mTLS handshake presents it. Needs DATABASE_URL,
//     GCP_KMS_KEY_NAME and GOOGLE_APPLICATION_CREDENTIALS pointed at whatever KMS key
//     actually wraps that certificate's DEK.
//
// Usage:
//
//	# password mode
//	EPROC_USERNAME=... EPROC_PASSWORD=... EPROC_CNJ=1234567-89.2026.8.26.0100 \
//	  go run ./cmd/eproc-dump
//
//	# certificate mode
//	DATABASE_URL=... GCP_KMS_KEY_NAME=... GOOGLE_APPLICATION_CREDENTIALS=... \
//	  EPROC_CERT_TENANT_ID=... EPROC_CERT_ID=... EPROC_CNJ=4013029-38.2026.8.26.0196 \
//	  go run ./cmd/eproc-dump
//
// Both modes accept: EPROC_PROXY_URL, EPROC_BASE_URL.
package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jusassessoria/platform/internal/certificate"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/eproc"
	"github.com/jusassessoria/platform/lib/events"
	"github.com/jusassessoria/platform/lib/transport"
)

func main() {
	if err := run(); err != nil {
		// The error is typed (apperr) and HTTP-agnostic; it never contains the credential.
		fmt.Fprintf(os.Stderr, "eproc-dump: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cnj := os.Getenv("EPROC_CNJ")
	if cnj == "" {
		return fmt.Errorf("EPROC_CNJ is required (the process number to dump)")
	}

	proxyURL, err := parseProxy(os.Getenv("EPROC_PROXY_URL"))
	if err != nil {
		return fmt.Errorf("EPROC_PROXY_URL: %w", err)
	}

	authOpt, clientCert, err := resolveAuth(ctx)
	if err != nil {
		return err
	}

	// Build the Chrome uTLS transport (with the optional residential/BR proxy and,
	// in certificate mode, the mTLS client cert), same anti-bot posture the DJEN
	// connector proved necessary. This is the ONE place that knows about
	// lib/transport; the eproc client stays pure.
	hc := &http.Client{
		Timeout:   90 * time.Second,
		Transport: transport.ChromeTransport(proxyURL, clientCert),
	}

	client := eproc.NewEprocClient(hc,
		authOpt,
		eproc.WithBaseURL(os.Getenv("EPROC_BASE_URL")), // empty keeps the default
	)

	fmt.Println("== eproc-dump (D0 spike) ==")
	fmt.Printf("proxy: %s\n", proxyLabel(proxyURL))
	fmt.Printf("cnj:   %s\n", cnj)
	fmt.Printf("auth:  %s\n", authLabel(clientCert))
	fmt.Println()

	// --- Login -----------------------------------------------------------------------
	// A challenge (captcha/MFA) surfaces as a typed Forbidden — one of the UNKNOWNs.
	challengeSeen := false
	if err := client.Login(ctx); err != nil {
		if eproc.IsForbidden(err) {
			challengeSeen = true
			fmt.Println("LOGIN: BLOCKED by captcha/MFA challenge")
		}
		return fmt.Errorf("login: %w", err)
	}
	fmt.Printf("LOGIN: ok — session status = %s\n", client.Status())
	fmt.Printf("CAPTCHA/MFA at login: %s\n", yesNo(challengeSeen))
	fmt.Println()

	// --- Process -----------------------------------------------------------------------
	proc, err := client.GetProcess(ctx, cnj)
	if err != nil {
		return fmt.Errorf("get process: %w", err)
	}
	fmt.Println("PROCESS:")
	fmt.Printf("  cnj:          %s\n", proc.CNJNumber)
	fmt.Printf("  class:        %s\n", proc.Class)
	fmt.Printf("  judging body: %s\n", proc.JudgingBody)
	fmt.Printf("  parties:      %d\n", len(proc.Parties))
	// The core UNKNOWN #1: print the RAW CPF/CNPJ field verbatim, plus a classification
	// of what shape it came in, so the human sees whether the portal leaks it.
	for i, p := range proc.Parties {
		fmt.Printf("    [%d] name=%q role=%q\n", i, p.Name, p.Role)
		fmt.Printf("        CPF/CNPJ raw=%q  → %s\n", p.RawDocument, classifyDocument(p.RawDocument))
	}
	fmt.Println()

	// --- Events ------------------------------------------------------------------------
	events, err := client.ListEvents(ctx, cnj)
	if err != nil {
		return fmt.Errorf("list events: %w", err)
	}
	fmt.Printf("EVENTS: %d\n", len(events))
	var firstDoc string
	for i, e := range events {
		fmt.Printf("    [%d] %s  %s  (%d docs)\n", i, e.Date.Format(time.RFC3339), e.Description, len(e.Documents))
		if firstDoc == "" && len(e.Documents) > 0 {
			firstDoc = e.Documents[0].ExternalID
		}
	}
	fmt.Println()

	// --- Download one document ---------------------------------------------------------
	if firstDoc == "" {
		fmt.Println("DOWNLOAD: skipped — no document found in the events")
	} else {
		path, n, err := downloadToTemp(ctx, client, firstDoc)
		if err != nil {
			return fmt.Errorf("download document %q: %w", firstDoc, err)
		}
		fmt.Printf("DOWNLOAD: %d bytes → %s\n", n, path)
	}
	fmt.Println()

	fmt.Println("== D0 findings ==")
	fmt.Printf("- CPF/CNPJ exposure: see the per-party raw values above.\n")
	fmt.Printf("- captcha/MFA seen at login: %s\n", yesNo(challengeSeen))
	fmt.Printf("- NOTE: portal paths and parsing are PROVISIONAL (Portão B) — if any step\n")
	fmt.Printf("  failed with a parse/unavailable error, adjust lib/eproc/wiring.go.\n")
	return nil
}

// resolveAuth picks the auth mode from the environment: certificate mode when
// EPROC_CERT_TENANT_ID/EPROC_CERT_ID are set, password mode otherwise. It returns the
// eproc.Option to configure the client plus the *tls.Certificate to hand the transport
// (nil in password mode).
func resolveAuth(ctx context.Context) (eproc.Option, *tls.Certificate, error) {
	tenantID := os.Getenv("EPROC_CERT_TENANT_ID")
	certID := os.Getenv("EPROC_CERT_ID")
	if tenantID == "" && certID == "" {
		username := os.Getenv("EPROC_USERNAME")
		password := os.Getenv("EPROC_PASSWORD")
		if username == "" || password == "" {
			return nil, nil, fmt.Errorf("set EPROC_USERNAME+EPROC_PASSWORD, or EPROC_CERT_TENANT_ID+EPROC_CERT_ID for certificate mode")
		}
		return eproc.WithCredentials(eproc.Credentials{Username: username, Password: password}), nil, nil
	}
	if tenantID == "" || certID == "" {
		return nil, nil, fmt.Errorf("EPROC_CERT_TENANT_ID and EPROC_CERT_ID must both be set")
	}

	clientCert, info, err := loadClientCertificate(ctx, tenantID, certID)
	if err != nil {
		return nil, nil, fmt.Errorf("load certificate from vault: %w", err)
	}
	fmt.Printf("certificate: CN=%q OAB=%q (loaded from vault, never printed: key material)\n", info.SubjectCN, info.OAB)
	return eproc.WithCertificateAuth(), clientCert, nil
}

// loadClientCertificate opens the KMS-backed certificate vault (internal/certificate)
// and builds a *tls.Certificate ready for mutual TLS: Certificate is the DER chain
// (leaf first, matching crypto/tls's own convention), PrivateKey is the KMS-backed
// crypto.Signer — the raw key never leaves the vault's process boundary, only sign
// operations do (see certificate.UseCase.NewSigner).
func loadClientCertificate(ctx context.Context, tenantID, certID string) (*tls.Certificate, certificate.SignerInfo, error) {
	dsn := os.Getenv("DATABASE_URL")
	kekName := os.Getenv("GCP_KMS_KEY_NAME")
	if dsn == "" || kekName == "" {
		return nil, certificate.SignerInfo{}, fmt.Errorf("DATABASE_URL and GCP_KMS_KEY_NAME are required in certificate mode")
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, certificate.SignerInfo{}, fmt.Errorf("pool: %w", err)
	}
	defer pool.Close()

	cipher, err := certificate.NewEnvelopeCipher(ctx, kekName)
	if err != nil {
		return nil, certificate.SignerInfo{}, fmt.Errorf("KMS envelope cipher: %w", err)
	}

	repo := certificate.NewRepository()
	uow := database.NewUnitOfWork(pool)
	uc := certificate.NewUseCase(repo, uow, cipher, events.NewOutbox())

	signer, leaf, intermediates, info, err := uc.NewSigner(ctx, tenantID, certID)
	if err != nil {
		return nil, certificate.SignerInfo{}, err
	}

	chain := make([][]byte, 0, 1+len(intermediates))
	chain = append(chain, leaf.Raw)
	for _, ic := range intermediates {
		chain = append(chain, ic.Raw)
	}
	return &tls.Certificate{
		Certificate: chain,
		PrivateKey:  signer,
		Leaf:        leaf,
	}, info, nil
}

// downloadToTemp streams the document to a temp file (never buffering it whole) and
// returns its path and byte count.
func downloadToTemp(ctx context.Context, client *eproc.Client, docID string) (string, int64, error) {
	f, err := os.CreateTemp("", "eproc-doc-*.bin")
	if err != nil {
		return "", 0, err
	}
	defer f.Close()

	n, err := client.DownloadDocument(ctx, docID, f)
	if err != nil {
		return "", 0, err
	}
	return filepath.Clean(f.Name()), n, nil
}

// parseProxy parses the optional proxy URL. Empty means direct connection.
func parseProxy(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, nil
	}
	return url.Parse(raw)
}

// proxyLabel describes the proxy WITHOUT leaking userinfo (password) into stdout.
func proxyLabel(u *url.URL) string {
	if u == nil {
		return "none (direct)"
	}
	// Redact userinfo — only the host is safe to print.
	return u.Scheme + "://" + u.Host
}

// authLabel describes which auth mode is active, without ever printing a secret.
func authLabel(clientCert *tls.Certificate) string {
	if clientCert != nil {
		return "certificate (mutual TLS)"
	}
	return "password"
}

// classifyDocument describes the shape of a CPF/CNPJ field so the human sees at a glance
// whether the portal exposed it, masked it, or omitted it — without us deciding for them.
func classifyDocument(raw string) string {
	switch {
	case raw == "":
		return "ABSENT (empty)"
	case strings.Contains(raw, "*") || strings.Contains(raw, "x") || strings.Contains(raw, "X"):
		return "MASKED (partially hidden)"
	default:
		return "CLEAR (fully exposed)"
	}
}

func yesNo(b bool) string {
	if b {
		return "YES"
	}
	return "no"
}

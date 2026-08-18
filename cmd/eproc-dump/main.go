// Command eproc-dump is the D0 spike tool (docs/erd-execucao-judicial-tjsp.md §10.2).
// It is what a human runs the moment a real lawyer credential is available (Portão B):
// it authenticates against the TJSP eproc portal, reuses the session, lists one process
// + its events, downloads one document to disk, and prints the evidence the spike needs
// to resolve its two UNKNOWNs —
//
//  1. does the portal expose CPF/CNPJ of parties (in clear, masked, or absent) without a
//     procuração? → we print the RAW document field of every party, verbatim.
//  2. does a captcha/MFA appear at login or in the autos? → we print whether the flow hit
//     a challenge.
//
// It reads everything from the environment; it NEVER logs the credential (not even in
// debug). The Chrome uTLS transport (lib/transport) is built HERE and injected into the
// pure lib/eproc client — the lib stays free of the transport dependency.
//
// Usage:
//
//	EPROC_USERNAME=... EPROC_PASSWORD=... \
//	  [EPROC_PROXY_URL=http://user:pass@host:port] \
//	  [EPROC_BASE_URL=https://eproc1g.tjsp.jus.br] \
//	  [EPROC_CNJ=1234567-89.2026.8.26.0100] \
//	  go run ./cmd/eproc-dump
package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jusassessoria/platform/lib/eproc"
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
	username := os.Getenv("EPROC_USERNAME")
	password := os.Getenv("EPROC_PASSWORD")
	if username == "" || password == "" {
		return fmt.Errorf("EPROC_USERNAME and EPROC_PASSWORD are required (never printed)")
	}

	cnj := os.Getenv("EPROC_CNJ")
	if cnj == "" {
		return fmt.Errorf("EPROC_CNJ is required (the process number to dump)")
	}

	// Build the Chrome uTLS transport (with the optional residential/BR proxy), same
	// anti-bot posture the DJEN connector proved necessary. This is the ONE place that
	// knows about lib/transport; the eproc client stays pure.
	proxyURL, err := parseProxy(os.Getenv("EPROC_PROXY_URL"))
	if err != nil {
		return fmt.Errorf("EPROC_PROXY_URL: %w", err)
	}
	hc := &http.Client{
		Timeout:   90 * time.Second,
		Transport: transport.ChromeTransport(proxyURL),
	}

	client := eproc.NewEprocClient(hc,
		eproc.WithCredentials(eproc.Credentials{Username: username, Password: password}),
		eproc.WithBaseURL(os.Getenv("EPROC_BASE_URL")), // empty keeps the default
	)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	fmt.Println("== eproc-dump (D0 spike) ==")
	fmt.Printf("proxy: %s\n", proxyLabel(proxyURL))
	fmt.Printf("cnj:   %s\n", cnj)
	fmt.Println()

	// --- Login -----------------------------------------------------------------------
	// A challenge (captcha/MFA) surfaces as a typed Forbidden — one of the UNKNOWNs.
	challengeSeen := false
	if err := client.Login(ctx); err != nil {
		if eproc.IsForbidden(err) {
			challengeSeen = true
			fmt.Println("LOGIN: BLOCKED by captcha/MFA challenge (UNKNOWN #2 answered: YES)")
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

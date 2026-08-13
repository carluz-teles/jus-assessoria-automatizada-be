package extraction

import (
	"context"
	"encoding/base64"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/jusassessoria/platform/lib/apperr"
)

// ocrModel is the Claude vision model the OCR adapter uses (repo directive: claude-opus-4-8).
// ocrVersion encodes adapter+model in extractor_version so a re-OCR is auditable and the
// indexer knows text quality provenance. ocrMaxTokens bounds one document's transcription.
const (
	ocrModel     = anthropic.ModelClaudeOpus4_8
	ocrVersion   = "claude-ocr-opus-4-8"
	ocrMaxTokens = 8192
)

// pageDelimiter is the sentinel the OCR prompt asks Claude to emit between pages, so a
// single whole-document transcription can be split back into per-page PageText (the pinned
// text contract is per-page). It is deliberately unusual so it never collides with document
// prose.
const pageDelimiter = "<<<PAGE_BREAK>>>"

// ocrPrompt instructs Claude to transcribe the PDF verbatim, page by page, separated by the
// delimiter — no commentary, no summarization, so the output is faithful text for chunk+embed.
const ocrPrompt = "Transcribe ALL text from this PDF document VERBATIM, exactly as it appears, " +
	"preserving reading order. Do NOT summarize, translate, or add any commentary. " +
	"Separate the text of each page with a line containing exactly " + pageDelimiter + " and nothing else. " +
	"Output ONLY the transcribed text."

// visionClient is the narrow port over the Anthropic Messages API the OCR adapter needs —
// one call. Depending on it (not the concrete *anthropic.Client) is what lets tests inject a
// fake and NEVER hit the real API. The real adapter (anthropicVision) wraps the SDK client.
type visionClient interface {
	Transcribe(ctx context.Context, base64PDF string) (string, error)
}

// OCRExtractor is the Fatia 6 adapter: for scanned PDFs (no text layer) it OCRs the document
// via Claude vision, sending the whole PDF as a base64 document block and asking for verbatim
// per-page text. It sits behind the same TextExtractor port; the vision call is behind the
// visionClient interface so tests use a fake. hasTextLayer is ALWAYS false from OCR — by
// definition OCR only runs on a document that had no usable text layer.
type OCRExtractor struct {
	client visionClient
}

// NewOCRExtractor wires the OCR adapter to a vision client. Production injects
// NewAnthropicVision(apiKey); tests inject a fake.
func NewOCRExtractor(client visionClient) *OCRExtractor {
	return &OCRExtractor{client: client}
}

var _ TextExtractor = (*OCRExtractor)(nil)

// Extract base64-encodes the PDF, asks the vision client to transcribe it, and splits the
// transcription into per-page PageText on the delimiter. A vision fault surfaces as-is
// (typed by the adapter — an auth/quota fault is retryable infra, a permanently-bad input
// invalid). The returned version is the OCR version; hasTextLayer is always false.
func (o *OCRExtractor) Extract(ctx context.Context, data []byte) ([]PageText, bool, string, error) {
	b64 := base64.StdEncoding.EncodeToString(data)
	out, err := o.client.Transcribe(ctx, b64)
	if err != nil {
		return nil, false, ocrVersion, err
	}
	return splitPages(out), false, ocrVersion, nil
}

// splitPages turns the delimiter-separated transcription into per-page PageText (1-based).
// A transcription with no delimiter is a single page. Empty text yields a single empty page
// so pages >= 1 (a document always has at least one page) — the caller's pages count never
// under-reports a real document as zero pages.
func splitPages(s string) []PageText {
	parts := strings.Split(s, pageDelimiter)
	pages := make([]PageText, 0, len(parts))
	for i, part := range parts {
		pages = append(pages, PageText{Page: i + 1, Text: strings.TrimSpace(part)})
	}
	return pages
}

// anthropicVision is the real visionClient over the Anthropic Go SDK. It holds the
// constructed client (safe for concurrent use).
type anthropicVision struct {
	client anthropic.Client
}

// NewAnthropicVision builds the real vision client from an API key. The key is injected
// (from the worker's config) — the adapter never reads the environment itself, keeping the
// slice free of config coupling. It is the production visionClient the OCR adapter wraps.
func NewAnthropicVision(apiKey string) visionClient {
	return &anthropicVision{client: anthropic.NewClient(option.WithAPIKey(apiKey))}
}

// Transcribe sends the base64 PDF as a document block plus the verbatim-OCR prompt and
// returns the concatenated text of the response's text blocks. An API fault is a typed infra
// error (retryable — auth/quota/network); a response with no text blocks is an invalid (a
// re-send yields the same empty result).
func (v *anthropicVision) Transcribe(ctx context.Context, base64PDF string) (string, error) {
	msg, err := v.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     ocrModel,
		MaxTokens: ocrMaxTokens,
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(
				anthropic.NewDocumentBlock(anthropic.Base64PDFSourceParam{Data: base64PDF}),
				anthropic.NewTextBlock(ocrPrompt),
			),
		},
	})
	if err != nil {
		return "", apperr.NewInfra("extraction: anthropic ocr", err)
	}

	var b strings.Builder
	for _, block := range msg.Content {
		if block.Text != "" {
			b.WriteString(block.Text)
		}
	}
	out := b.String()
	if strings.TrimSpace(out) == "" {
		return "", apperr.NewInvalid("extraction: anthropic ocr returned no text")
	}
	return out, nil
}

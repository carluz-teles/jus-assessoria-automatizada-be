package extraction

import (
	"bytes"
	"context"
	"strings"

	"golang.org/x/net/html"

	"github.com/jusassessoria/platform/lib/apperr"
)

// htmlTextVersion identifies the HTML text adapter in extractor_version. Some eproc
// documents (and access/error pages returned under a "pdf" mime) are actually HTML;
// routing them here instead of the PDF reader keeps them out of the FAILED bucket and
// feeds their text to the RAG corpus.
const htmlTextVersion = "htmltext-v1"

// HTMLExtractor implements TextExtractor for HTML bytes: it parses with the WHATWG-spec
// x/net/html parser (already a dependency), walks the DOM collecting visible text (skipping
// script/style/head noise), and emits it as a single page (HTML has no page model). Stateless.
type HTMLExtractor struct{}

// NewHTMLExtractor returns the HTML adapter. Stateless; the worker injects the zero value.
func NewHTMLExtractor() *HTMLExtractor { return &HTMLExtractor{} }

var _ TextExtractor = (*HTMLExtractor)(nil)

// Extract renders an HTML document's visible text. hasTextLayer is true whenever any text was
// found (so the dispatcher returns it directly, never routing HTML bytes to the PDF OCR path);
// an unparseable body is a terminal invalid.
func (*HTMLExtractor) Extract(_ context.Context, data []byte) ([]PageText, bool, string, error) {
	doc, err := html.Parse(bytes.NewReader(data))
	if err != nil {
		return nil, false, htmlTextVersion, apperr.NewInvalid("extraction: parse html: " + err.Error())
	}

	var sb strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "script", "style", "head", "noscript":
				return // non-content subtrees
			case "br":
				sb.WriteByte('\n')
			}
		}
		if n.Type == html.TextNode {
			if t := strings.TrimSpace(n.Data); t != "" {
				sb.WriteString(t)
				sb.WriteByte(' ')
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		if n.Type == html.ElementNode && htmlBlockElement[n.Data] {
			sb.WriteByte('\n')
		}
	}
	walk(doc)

	// Collapse whitespace per line (matches the PDF extractor's clean output shape).
	lines := strings.Split(sb.String(), "\n")
	var out strings.Builder
	for _, ln := range lines {
		if collapsed := strings.Join(strings.Fields(ln), " "); collapsed != "" {
			out.WriteString(collapsed)
			out.WriteByte('\n')
		}
	}
	text := strings.TrimSpace(out.String())
	return []PageText{{Page: 1, Text: text}}, text != "", htmlTextVersion, nil
}

// htmlBlockElement marks tags after whose subtree a newline improves readability (so a
// paragraph or table row doesn't run into the next). Inline tags (span, a, b, …) are omitted.
var htmlBlockElement = map[string]bool{
	"p": true, "div": true, "tr": true, "li": true, "table": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"section": true, "article": true, "header": true, "footer": true,
}

// looksLikeHTML sniffs whether bytes are HTML rather than a PDF — the extractor can't trust the
// stored mime_type (eproc serves HTML access/error pages under a "pdf" mime). A "%PDF" magic
// wins for PDF; otherwise a document that begins (after leading whitespace/BOM) with an HTML
// marker is treated as HTML. Anything else falls through to the PDF reader (which rejects it as
// invalid, the pre-existing behavior for unknown formats).
func looksLikeHTML(data []byte) bool {
	trimmed := bytes.TrimLeft(data, " \t\r\n\ufeff")
	if bytes.HasPrefix(trimmed, []byte("%PDF")) {
		return false
	}
	head := trimmed
	if len(head) > 1024 {
		head = head[:1024]
	}
	lower := bytes.ToLower(head)
	return bytes.Contains(lower, []byte("<!doctype html")) ||
		bytes.Contains(lower, []byte("<html")) ||
		bytes.Contains(lower, []byte("<body")) ||
		bytes.Contains(lower, []byte("<head"))
}

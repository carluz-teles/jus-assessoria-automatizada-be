package acquisition

import (
	"strings"

	"golang.org/x/net/html"
)

// htmlPlaintext converts the DJEN intimation teor (stored as HTML) into plain
// text for the content_preview field. It uses the golang.org/x/net/html tokenizer
// to extract text nodes, which handles entity decoding (e.g. &Ccedil;→Ç,
// &Atilde;→Ã, &ordm;→º) correctly without regex fragility.
//
// Strategy:
//  - <script> and <style> nodes and all their descendants are skipped.
//  - Block-level elements (p, div, br, tr, li, hN, article, section, table,
//    thead, tbody) inject a newline break so paragraph structure survives the tag strip.
//  - All other tags are dropped; their text children are kept.
//  - Whitespace within a line is collapsed to a single space; blank lines are
//    squeezed to at most one blank line.
//
// Input that contains no HTML ('<') or entities ('&') is returned trimmed and
// unchanged — the fast path avoids the tokenizer overhead for plain-text content.
func htmlPlaintext(s string) string {
	if !strings.ContainsAny(s, "<&") {
		return strings.TrimSpace(s)
	}

	var b strings.Builder
	tok := html.NewTokenizer(strings.NewReader(s))
	skip := 0 // nesting depth inside a skipped subtree (script/style)

	for {
		tt := tok.Next()
		if tt == html.ErrorToken {
			break
		}
		switch tt {
		case html.StartTagToken, html.SelfClosingTagToken:
			name, _ := tok.TagName()
			tag := string(name)
			if skip > 0 {
				// Inside a skipped subtree — track nesting so nested script/style
				// does not prematurely end the skip.
				if tt == html.StartTagToken {
					skip++
				}
				continue
			}
			if tag == "script" || tag == "style" {
				skip++
				continue
			}
			if isBlockTag(tag) {
				// Inject a newline so block structure survives the strip.
				b.WriteByte('\n')
			}
		case html.EndTagToken:
			if skip > 0 {
				skip--
				continue
			}
			name, _ := tok.TagName()
			if isBlockTag(string(name)) {
				b.WriteByte('\n')
			}
		case html.TextToken:
			if skip > 0 {
				continue
			}
			b.WriteString(tok.Token().Data)
		}
	}

	return collapseWhitespace(b.String())
}

// isBlockTag reports whether the lowercased tag name is a block-level element
// that should become a newline boundary in the plaintext output.
func isBlockTag(tag string) bool {
	switch tag {
	case "p", "div", "br", "tr", "li", "article", "section", "table", "thead", "tbody",
		"h1", "h2", "h3", "h4", "h5", "h6", "th", "td":
		return true
	default:
		return false
	}
}

// collapseWhitespace trims each line, collapses intra-line space/tab runs to a
// single space, and squeezes runs of 3+ newlines to at most 2 (one blank line).
func collapseWhitespace(s string) string {
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		// Collapse runs of spaces/tabs within the line.
		lines[i] = strings.Join(strings.Fields(ln), " ")
	}
	s = strings.Join(lines, "\n")

	// Squeeze 3+ consecutive newlines to 2 (at most one blank line between paragraphs).
	for strings.Contains(s, "\n\n\n") {
		s = strings.ReplaceAll(s, "\n\n\n", "\n\n")
	}
	return strings.TrimSpace(s)
}

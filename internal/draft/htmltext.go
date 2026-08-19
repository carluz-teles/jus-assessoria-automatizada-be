package draft

import (
	"html"
	"regexp"
	"strings"
)

// stripHTML converts the DJEN intimation teor (stored as HTML) into plain text for
// LLM prompt injection. It is deliberately minimal and regex-based — NOT a
// general-purpose sanitizer: it drops <script>/<style> blocks, turns block-level
// breaks into newlines, removes all remaining tags, unescapes HTML entities, and
// collapses whitespace. Scoped to the semi-structured DJEN content; input that is
// already plain text (no '<') is returned trimmed, unchanged.
func stripHTML(s string) string {
	if !strings.ContainsAny(s, "<&") {
		return strings.TrimSpace(s)
	}
	s = htmlScriptStyleRe.ReplaceAllString(s, "")
	s = htmlBreakRe.ReplaceAllString(s, "\n")
	s = htmlTagRe.ReplaceAllString(s, "")
	s = html.UnescapeString(s)

	// Per-line trim + collapse intra-line runs of spaces, then squeeze blank lines.
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		lines[i] = strings.TrimSpace(htmlSpacesRe.ReplaceAllString(ln, " "))
	}
	s = strings.Join(lines, "\n")
	s = htmlBlankLinesRe.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

var (
	// <script>…</script> and <style>…</style>, including their content (?is = dotall+case-insensitive).
	htmlScriptStyleRe = regexp.MustCompile(`(?is)<(script|style)\b[^>]*>.*?</(script|style)>`)
	// Block-level breaks become newlines so paragraph/list structure survives the tag strip.
	htmlBreakRe = regexp.MustCompile(`(?i)<br\s*/?>|</(p|div|tr|li|h[1-6]|article|section|table|thead|tbody)\s*>`)
	// Any remaining tag (?s = dotall so multi-line tags are matched).
	htmlTagRe = regexp.MustCompile(`(?s)<[^>]+>`)
	// Runs of spaces/tabs within a line.
	htmlSpacesRe = regexp.MustCompile(`[ \t]+`)
	// 3+ newlines collapse to a blank line.
	htmlBlankLinesRe = regexp.MustCompile(`\n{3,}`)
)

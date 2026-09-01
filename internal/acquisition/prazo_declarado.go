package acquisition

import "regexp"

// prazo_declarado.go extracts a declared day-count prazo from an intimação's free-text
// teor, deterministically — regex, never an LLM (the design's philosophy keeps IA
// scoped to classifying the TIPO of an omissa intimação, never to extracting a prazo;
// docs/design-motor-de-prazos-v1.md §"Fallback IA"). The result feeds
// IntimationObserved.PrazoDeclarado, whose format ("<N> dias") is what
// internal/deadline's parseDeclaradoDays (domain.go) already expects — only the leading
// integer is read there.

// prazoDeclaradoPatterns are tried in order against the plaintext teor; the FIRST match
// wins. Compiled once at package init (never per-call). Each has exactly one capture
// group: the day count.
//
//   - "no prazo de N (por extenso) dias" — the most common explicit phrasing.
//   - "prazo de N (por extenso) dias" — the same, without the leading "no".
//   - "prazo de N dias úteis|corridos" — an explicit útil/corrido suffix instead of a
//     parenthetical.
var prazoDeclaradoPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)no\s+prazo\s+de\s+(\d+)\s*(?:\([^)]*\))?\s*dias?`),
	regexp.MustCompile(`(?i)prazo\s+de\s+(\d+)\s*(?:\([^)]*\))?\s*dias?`),
	regexp.MustCompile(`(?i)prazo\s+de\s+(\d+)\s*dias?\s+(?:úteis|corridos)`),
}

// extractPrazoDeclarado runs prazoDeclaradoPatterns over the teor's plaintext
// (htmlPlaintext strips the DJEN HTML wrapper first) and returns "<N> dias" for the
// first match, or "" when no pattern matches — it never guesses a number.
func extractPrazoDeclarado(teor string) string {
	plain := htmlPlaintext(teor)
	for _, re := range prazoDeclaradoPatterns {
		if m := re.FindStringSubmatch(plain); m != nil {
			return m[1] + " dias"
		}
	}
	return ""
}

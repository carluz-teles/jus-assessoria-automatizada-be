package draft

import (
	"regexp"
	"strings"
)

// structparse.go is the server-side fallback for the FE's structparse (Peça v2):
// when GET /v1/pecas/:id lands on a draft still holding plain-text `content`
// (created before migration 0056 / Fatia B), the read model parses the content
// into a *StructuredContent on the fly so the response matches the new shape;
// the repo then best-effort writes it back so subsequent reads skip the parser.
//
// Heurística: uma linha (não parágrafo) que casa com o cabeçalho romano
// ("I — Dos fatos", "II - Do direito"…) abre uma nova seção. Tudo antes do
// primeiro cabeçalho é preâmbulo. Aceita travessão em qualquer forma (— – -).
// Mantém a mesma regex do FE (features/pecas-v2/components/editor/section-parser.ts)
// pra os dois lados sempre produzirem a mesma estrutura.

var headingRE = regexp.MustCompile(`^\s*(I{1,3}|IV|V|VI{0,3}|IX|X)\s*[—–-]\s*(.+?)\s*$`)

// ParseStructured decodes the plain-text `content` column into a
// *StructuredContent (preamble + N sections). Returns nil when content has
// no paragraphs (empty or whitespace-only) — the caller keeps
// structured_content as NULL and the FE renders the empty state.
// A content that has no roman-heading lines returns a single-block
// StructuredContent (everything in the preamble, no sections) — degraded
// but usable; the FE handles both.
func ParseStructured(content string) *StructuredContent {
	paragraphs := splitParagraphs(content)
	if len(paragraphs) == 0 {
		return nil
	}

	out := &StructuredContent{
		Preamble: StructuredPreamble{Paragraphs: []string{}},
		Sections: []StructuredSection{},
	}

	var current *StructuredSection
	for _, p := range paragraphs {
		first := firstLine(p)
		if m := headingRE.FindStringSubmatch(first); m != nil {
			// Fecha a seção anterior (se houver) e abre uma nova.
			roman := m[1]
			title := strings.TrimSpace(m[2])
			title = normalizeTitle(title)
			sec := StructuredSection{
				ID:         slugTitle(title),
				Roman:      roman,
				Title:      title,
				ShortTitle: shortTitleOf(title),
				Paragraphs: []string{},
			}
			out.Sections = append(out.Sections, sec)
			current = &out.Sections[len(out.Sections)-1]

			// Se o parágrafo tem conteúdo APÓS o heading (linha continuada),
			// entra como primeiro parágrafo da seção.
			rest := strings.TrimSpace(strings.TrimPrefix(p, first))
			if rest != "" {
				current.Paragraphs = append(current.Paragraphs, rest)
			}
			continue
		}
		if current != nil {
			current.Paragraphs = append(current.Paragraphs, p)
		} else {
			out.Preamble.Paragraphs = append(out.Preamble.Paragraphs, p)
		}
	}

	return out
}

// splitParagraphs quebra o content em parágrafos separados por linhas em
// branco (uma ou mais). Filtra parágrafos que ficaram vazios após o trim.
func splitParagraphs(content string) []string {
	parts := regexp.MustCompile(`\n{2,}`).Split(content, -1)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// firstLine returns the first non-empty line of a paragraph — the heading
// candidate. Multi-line paragraphs (heading + continuation) usam a linha 1.
func firstLine(p string) string {
	if idx := strings.IndexByte(p, '\n'); idx >= 0 {
		return strings.TrimSpace(p[:idx])
	}
	return strings.TrimSpace(p)
}

// normalizeTitle preserves the "Dos "/"Da " article when present (mimicking
// what the LLM produces) but trims stray whitespace/punctuation.
func normalizeTitle(t string) string {
	return strings.TrimSpace(t)
}

// slugTitle converts "Dos fatos" → "fatos", "Do direito" → "direito", stripping
// articles and lowercasing. Falls back to the whole slug for exotic titles.
// The FE's slugTitle uses the same rule so IDs match across both sides.
func slugTitle(title string) string {
	base := shortTitleOf(title)
	base = strings.ToLower(base)
	base = removeDiacritics(base)
	// Only [a-z0-9] survives; runs of separators collapse to a single dash.
	var b strings.Builder
	prevDash := false
	for _, r := range base {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevDash = false
			continue
		}
		if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// shortTitleOf strips the leading article ("Dos fatos" → "Fatos") for chip
// display. Case-insensitive.
func shortTitleOf(title string) string {
	trimmed := strings.TrimSpace(title)
	lower := strings.ToLower(trimmed)
	for _, prefix := range []string{"dos ", "das ", "do ", "da "} {
		if strings.HasPrefix(lower, prefix) {
			return strings.TrimSpace(trimmed[len(prefix):])
		}
	}
	return trimmed
}

// removeDiacritics folds "ç" → "c", "á" → "a", etc. Minimal table covering
// Portuguese; the slug never surfaces to the user (only in URLs / chip keys)
// so completeness isn't critical.
func removeDiacritics(s string) string {
	repl := strings.NewReplacer(
		"á", "a", "à", "a", "ã", "a", "â", "a", "ä", "a",
		"é", "e", "è", "e", "ê", "e", "ë", "e",
		"í", "i", "ì", "i", "î", "i", "ï", "i",
		"ó", "o", "ò", "o", "õ", "o", "ô", "o", "ö", "o",
		"ú", "u", "ù", "u", "û", "u", "ü", "u",
		"ç", "c",
	)
	return repl.Replace(s)
}

package draft

import (
	"sort"
	"strings"
)

// theses_segments.go maps each selected thesis to the SECTION of the generated
// peça it produced (thesis↔segment, migration 0095). It is a PURE function over
// the already-parsed StructuredContent — no DB, no LLM — so it is deterministic
// and unit-testable. The generation use case calls it after markdownToHTML +
// parseHTMLToStructured and persists the matches in the same success tx.
//
// Strategy: match a thesis's label tokens against each section's TITLE + BODY. We
// match on the body (not just the title) because the model varies its structure —
// sometimes a per-thesis subheading ("DA EXTINÇÃO DO PROCESSO…", title carries the
// label), sometimes the thesis is a paragraph under a generic profile heading
// ("I — DAS PRELIMINARES", the label words live in the body: "impõe-se a extinção
// … inércia do exequente"). Matching title+body covers both. This is robust — zero
// dependency on the model emitting a special format (the reason we avoided inline
// markers, which is exactly what the truncation fights).

// segmentMatchThreshold is the minimum fraction of a thesis's (normalized) label
// tokens that a section's title+body must cover to be considered its segment. 0.6
// keeps "Extinção do processo por inércia do exequente" → the PRELIMINARES section
// whose body cites all four terms (full cover) while rejecting a section about an
// unrelated argument (near-zero cover).
const segmentMatchThreshold = 0.6

// segmentStopwords are Portuguese function words dropped before matching — they
// carry no discriminative signal and would inflate spurious overlaps.
var segmentStopwords = map[string]bool{
	"a": true, "o": true, "as": true, "os": true, "um": true, "uma": true,
	"de": true, "da": true, "do": true, "das": true, "dos": true,
	"e": true, "em": true, "no": true, "na": true, "nos": true, "nas": true,
	"por": true, "para": true, "com": true, "que": true, "ao": true, "aos": true,
	"à": true, "às": true, "sobre": true, "sem": true,
}

// thesisSegmentMatch pairs a thesis id with the segment (section) it produced.
type thesisSegmentMatch struct {
	ThesisID string
	Segment  ThesisSegment
}

// matchThesisSegments returns, for each thesis that matched a section, the section
// text as its segment. The match is 1:1 (greedy by descending token coverage): a
// section belongs to at most one thesis and a thesis to at most one section, so a
// generic "DOS FATOS" is not double-assigned. Returns nil when there is nothing to
// match — the generation then simply persists no segments (FE fallback).
func matchThesisSegments(theses []SuggestedThesis, sc *StructuredContent) []thesisSegmentMatch {
	if sc == nil || len(sc.Sections) == 0 || len(theses) == 0 {
		return nil
	}

	thesisTokens := make([][]string, len(theses))
	for i, t := range theses {
		thesisTokens[i] = normalizeTokens(t.Label)
	}
	// Match target = title + body: the label words may live in the heading OR in
	// the section's paragraphs (see strategy note above).
	sectionTokens := make([][]string, len(sc.Sections))
	for i, s := range sc.Sections {
		sectionTokens[i] = normalizeTokens(s.Title + " " + strings.Join(s.Paragraphs, " "))
	}

	type pair struct {
		ti, si int
		score  float64
	}
	var pairs []pair
	for ti := range theses {
		if len(thesisTokens[ti]) == 0 {
			continue
		}
		for si := range sc.Sections {
			if s := tokenCoverage(thesisTokens[ti], sectionTokens[si]); s >= segmentMatchThreshold {
				pairs = append(pairs, pair{ti: ti, si: si, score: s})
			}
		}
	}
	// Greedy 1:1 by descending score (ties: lower thesis index, then lower section).
	sort.SliceStable(pairs, func(a, b int) bool {
		if pairs[a].score != pairs[b].score {
			return pairs[a].score > pairs[b].score
		}
		if pairs[a].ti != pairs[b].ti {
			return pairs[a].ti < pairs[b].ti
		}
		return pairs[a].si < pairs[b].si
	})

	usedT := make([]bool, len(theses))
	usedS := make([]bool, len(sc.Sections))
	// Collect matches keyed by thesis index so the output can be ordered by thesis
	// position (stable, deterministic) regardless of match discovery order.
	matchByThesis := make(map[int]ThesisSegment)
	for _, p := range pairs {
		if usedT[p.ti] || usedS[p.si] {
			continue
		}
		usedT[p.ti] = true
		usedS[p.si] = true
		s := sc.Sections[p.si]
		matchByThesis[p.ti] = ThesisSegment{
			Heading:  sectionHeading(s),
			Conteudo: strings.Join(s.Paragraphs, "\n\n"),
		}
	}
	if len(matchByThesis) == 0 {
		return nil
	}

	out := make([]thesisSegmentMatch, 0, len(matchByThesis))
	pos := 0
	for ti := range theses {
		seg, ok := matchByThesis[ti]
		if !ok {
			continue
		}
		seg.Position = pos
		pos++
		out = append(out, thesisSegmentMatch{ThesisID: theses[ti].ID, Segment: seg})
	}
	return out
}

// sectionHeading renders a section's display heading: "ROMAN — Title" when the
// section carries a roman numeral, else just the title.
func sectionHeading(s StructuredSection) string {
	if s.Roman != "" && s.Title != "" {
		return s.Roman + " — " + s.Title
	}
	if s.Title != "" {
		return s.Title
	}
	return s.Roman
}

// tokenCoverage is the fraction of want's tokens present in have (as a set). It is
// asymmetric on purpose — a long section heading that CONTAINS the whole thesis
// label scores 1.0, which is what we want (the section is the thesis's home).
func tokenCoverage(want, have []string) float64 {
	if len(want) == 0 {
		return 0
	}
	set := make(map[string]bool, len(have))
	for _, h := range have {
		set[h] = true
	}
	hit := 0
	for _, w := range want {
		if set[w] {
			hit++
		}
	}
	return float64(hit) / float64(len(want))
}

// normalizeTokens lowercases, strips accents, drops punctuation and stopwords, and
// splits into meaningful tokens (len >= 3, to skip noise like "ii"/"ao" survivors).
func normalizeTokens(s string) []string {
	folded := stripAccentsLower(s)
	fields := strings.FieldsFunc(folded, func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if len(f) < 3 || segmentStopwords[f] {
			continue
		}
		out = append(out, f)
	}
	return out
}

// stripAccentsLower lowercases and folds common Portuguese accented runes to ASCII
// so "Extinção" and "EXTINÇÃO" tokenize identically. Kept intentionally small (no
// unicode/norm dependency) — it covers the accents that appear in legal headings.
func stripAccentsLower(s string) string {
	var sb strings.Builder
	sb.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		switch r {
		case 'á', 'à', 'â', 'ã', 'ä':
			sb.WriteRune('a')
		case 'é', 'è', 'ê', 'ë':
			sb.WriteRune('e')
		case 'í', 'ì', 'î', 'ï':
			sb.WriteRune('i')
		case 'ó', 'ò', 'ô', 'õ', 'ö':
			sb.WriteRune('o')
		case 'ú', 'ù', 'û', 'ü':
			sb.WriteRune('u')
		case 'ç':
			sb.WriteRune('c')
		case 'ñ':
			sb.WriteRune('n')
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

package extraction

import "testing"

import ledongpdf "github.com/ledongthuc/pdf"

type pdfTextHorizontal = ledongpdf.TextHorizontal
type pdfText = ledongpdf.Text

// TestHasUsableTextLayer exercises the density rule that routes a stamped scan to OCR while
// keeping a real text document's text layer. The pure helper is tested directly (no PDF
// fixture): the decision is total/numPages >= minCharsPerPage, guarding a 0-page document.
func TestHasUsableTextLayer(t *testing.T) {
	tests := []struct {
		name     string
		total    int
		numPages int
		want     bool
	}{
		{
			// The prod case: a 71-page scan carrying only a thin PJe header stamp per page.
			// ~2100 chars total defeats any fixed total floor, but ~66 chars/page is well
			// below minCharsPerPage → OCR.
			name:     "stamped scan (66 chars/page) → no text layer",
			total:    66 * 71,
			numPages: 71,
			want:     false,
		},
		{
			name:     "dense text document (800 chars/page) → has text layer",
			total:    800 * 40,
			numPages: 40,
			want:     true,
		},
		{
			name:     "exactly at the floor → has text layer",
			total:    minCharsPerPage * 3,
			numPages: 3,
			want:     true,
		},
		{
			name:     "one below the floor → no text layer",
			total:    minCharsPerPage*3 - 1,
			numPages: 3,
			want:     false,
		},
		{
			name:     "zero pages → no text layer (guards the division)",
			total:    500,
			numPages: 0,
			want:     false,
		},
		{
			name:     "empty scan (no text at all) → no text layer",
			total:    0,
			numPages: 10,
			want:     false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasUsableTextLayer(tt.total, tt.numPages); got != tt.want {
				t.Errorf("hasUsableTextLayer(%d, %d) = %v, want %v", tt.total, tt.numPages, got, tt.want)
			}
		})
	}
}

// --- joinFragments (the v2 de-spacing / anti-merge join) ---------------------

func TestJoinFragments(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		frag []string
		want string
	}{
		{"glyph run stays joined (fixes v1 char-spacing)", []string{"e", "n", "d", "e", "r", "e", "ç", "o"}, "endereço"},
		{"word fragments with trailing spaces", []string{"EXCELENTÍSSIMO ", "(A) ", "SENHOR "}, "EXCELENTÍSSIMO (A) SENHOR "},
		{"explicit space fragments between words", []string{"TRIBUNAL", "", " ", "", "DE"}, "TRIBUNAL DE"},
		{"back-to-back words get a boundary space (fixes merge)", []string{"PAULO", "COMARCA"}, "PAULO COMARCA"},
		{"glyph run then a word gets a boundary space", []string{"e", "n", "d", "da"}, "end da"},
		{"empty fragments are skipped", []string{"", "abc", "", "def"}, "abc def"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			frags := make(pdfTextHorizontal, 0, len(tt.frag))
			for _, s := range tt.frag {
				frags = append(frags, pdfText{S: s})
			}
			if got := joinFragments(frags); got != tt.want {
				t.Errorf("joinFragments(%q) = %q, want %q", tt.frag, got, tt.want)
			}
		})
	}
}

func TestDespaceRuns(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, in, want string }{
		{"long char-spaced word collapses", "e n d e r e ç o", "endereço"},
		{"char-spaced run then multi-space boundary keeps next word", "e n d e r e ç o   Executada", "endereço   Executada"},
		{"normal text untouched", "TRIBUNAL DE JUSTIÇA DO ESTADO", "TRIBUNAL DE JUSTIÇA DO ESTADO"},
		{"short single-char sequence stays (under floor)", "a e i", "a e i"},
		{"char-spaced with punctuation/digits", "C P F : 1 2 3", "CPF:123"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := despaceRuns(tt.in); got != tt.want {
				t.Errorf("despaceRuns(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

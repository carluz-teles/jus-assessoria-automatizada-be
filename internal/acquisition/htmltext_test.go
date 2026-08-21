package acquisition

import "testing"

// TestHtmlPlaintext proves the htmlPlaintext helper strips HTML tags, decodes
// entities, collapses whitespace, and handles edge cases correctly.
func TestHtmlPlaintext(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "plain text untouched",
			in:   "Fica intimada a parte.",
			want: "Fica intimada a parte.",
		},
		{
			name: "empty string",
			in:   "",
			want: "",
		},
		{
			name: "simple tags stripped",
			in:   "<p>Ficam <b>as partes</b> intimadas</p>",
			want: "Ficam as partes intimadas",
		},
		{
			name: "Portuguese entities decoded",
			in:   "Jo&atilde;o &amp; Maria &lt;3 Jo&ccedil;a &Ccedil;ara&ccedil;a &Atilde;o &ordm;",
			want: "João & Maria <3 Joça Çaraça Ão º",
		},
		{
			name: "script and style blocks dropped with content",
			in:   "<style>.x{color:red}</style><p>Texto</p><script>alert(1)</script>",
			want: "Texto",
		},
		{
			// <p>...</p> emits a newline at the opening AND closing tag, so adjacent
			// paragraphs are separated by a blank line (two newlines), not one.
			name: "block elements become newlines",
			in:   "<html><body><p>Primeiro</p><p>Segundo</p></body></html>",
			want: "Primeiro\n\nSegundo",
		},
		{
			name: "whitespace collapsed within lines",
			in:   "<p>a    b\t\tc</p>",
			want: "a b c",
		},
		{
			name: "nested tags stripped, text preserved",
			in:   "<div><span><b>Texto</b> mais <em>texto</em></span></div>",
			want: "Texto mais texto",
		},
		{
			// Each th/td emits newlines at open+close, so cells are separated by blank
			// lines after whitespace collapse. The important property is all text is present.
			name: "table structure survives as newlines",
			in:   "<table><thead><tr><th>Col1</th><th>Col2</th></tr></thead><tbody><tr><td>A</td><td>B</td></tr></tbody></table>",
			want: "Col1\n\nCol2\n\nA\n\nB",
		},
		{
			name: "br becomes newline",
			in:   "Linha 1<br/>Linha 2<br>Linha 3",
			want: "Linha 1\nLinha 2\nLinha 3",
		},
		{
			name: "entities in DJEN intimation pattern",
			in:   "Ficam <b>intimadas</b> as partes para apresentar manifesta&ccedil;&atilde;o no prazo de 15 (quinze) dias &uacute;teis.",
			want: "Ficam intimadas as partes para apresentar manifestação no prazo de 15 (quinze) dias úteis.",
		},
		{
			name: "already plain text with trailing spaces trimmed",
			in:   "  texto simples  ",
			want: "texto simples",
		},
		{
			name: "nested script not prematurely ended by inner element",
			in:   "<script>if(a<b){c}</script><p>Visible</p>",
			want: "Visible",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := htmlPlaintext(tt.in); got != tt.want {
				t.Errorf("htmlPlaintext(%q)\n got  %q\n want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestContentPreview proves the combined strip+truncate behaviour of contentPreview:
// the preview is computed on plain text (tags/entities already stripped) and
// respects the contentPreviewLen rune limit.
func TestContentPreview(t *testing.T) {
	t.Parallel()

	t.Run("html stripped before truncation", func(t *testing.T) {
		t.Parallel()
		in := "<p>Texto</p>"
		got := contentPreview(in)
		if got != "Texto" {
			t.Errorf("contentPreview(%q) = %q, want %q", in, got, "Texto")
		}
	})

	t.Run("truncates at rune boundary not byte", func(t *testing.T) {
		t.Parallel()
		// Build a plain-text string longer than contentPreviewLen runes,
		// using Portuguese characters so bytes != runes.
		var b []rune
		for i := 0; i < contentPreviewLen+10; i++ {
			b = append(b, 'ã')
		}
		in := string(b)
		got := contentPreview(in)
		if len([]rune(got)) != contentPreviewLen {
			t.Errorf("len(rune(preview)) = %d, want %d", len([]rune(got)), contentPreviewLen)
		}
	})

	t.Run("short html not truncated", func(t *testing.T) {
		t.Parallel()
		in := "<b>Olá</b>"
		got := contentPreview(in)
		if got != "Olá" {
			t.Errorf("contentPreview(%q) = %q, want %q", in, got, "Olá")
		}
	})

	t.Run("entity counted as one rune after decode", func(t *testing.T) {
		t.Parallel()
		// ã is one rune; &atilde; is 7 bytes that decode to 1 rune.
		// A preview of 1 char HTML entity vs plain: same rune count.
		in := "&atilde;"
		got := contentPreview(in)
		if got != "ã" {
			t.Errorf("contentPreview(%q) = %q, want \"ã\"", in, got)
		}
	})
}

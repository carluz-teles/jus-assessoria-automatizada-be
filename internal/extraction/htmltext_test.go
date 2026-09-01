package extraction

import (
	"context"
	"strings"
	"testing"
)

func TestHTMLExtractor_ExtractsVisibleTextSkippingNoise(t *testing.T) {
	t.Parallel()

	const doc = `<!DOCTYPE html><html><head><title>ignorar</title>
<style>.x{color:red}</style><script>var a=1;</script></head>
<body><h1>Petição</h1><p>O réu   confessou a dívida.</p>
<div>Segundo parágrafo</div></body></html>`

	pages, hasText, version, err := (&HTMLExtractor{}).Extract(context.Background(), []byte(doc))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !hasText {
		t.Fatal("hasText = false, want true")
	}
	if version != htmlTextVersion {
		t.Errorf("version = %q, want %q", version, htmlTextVersion)
	}
	if len(pages) != 1 || pages[0].Page != 1 {
		t.Fatalf("pages = %+v, want a single page 1", pages)
	}
	text := pages[0].Text
	// Head/style/script content must be gone; body text present with collapsed spaces.
	for _, forbidden := range []string{"ignorar", "color:red", "var a=1"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("text leaked non-content %q: %q", forbidden, text)
		}
	}
	if !strings.Contains(text, "Petição") || !strings.Contains(text, "O réu confessou a dívida.") {
		t.Errorf("text missing body content: %q", text)
	}
	if !strings.Contains(text, "Segundo parágrafo") {
		t.Errorf("text missing second block: %q", text)
	}
}

func TestHTMLExtractor_EmptyBodyHasNoTextLayer(t *testing.T) {
	t.Parallel()
	pages, hasText, _, err := (&HTMLExtractor{}).Extract(context.Background(), []byte("<html><body>   </body></html>"))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if hasText {
		t.Error("hasText = true for an empty body, want false")
	}
	if len(pages) != 1 || pages[0].Text != "" {
		t.Errorf("pages = %+v, want one empty page", pages)
	}
}

func TestLooksLikeHTML(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		data string
		want bool
	}{
		{"pdf magic", "%PDF-1.4\n...", false},
		{"pdf magic wins over angle brackets", "%PDF-1.4 <html>", false},
		{"doctype html", "<!DOCTYPE html><html>...", true},
		{"html tag", "\n\n  <html lang=\"pt\">", true},
		{"body tag", "<body>oi</body>", true},
		{"leading whitespace then html", "   \t\n<HTML>", true},
		{"plain text is not html", "apenas texto solto", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := looksLikeHTML([]byte(tt.data)); got != tt.want {
				t.Errorf("looksLikeHTML(%q) = %v, want %v", tt.data, got, tt.want)
			}
		})
	}
}

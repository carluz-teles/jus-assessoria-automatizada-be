package draft

import "testing"

// TestParseHTMLToStructured_PlainParagraphHeading covers the production bug:
// the LLM sometimes emits a section title as a plain "<p>I – DOS FATOS</p>"
// instead of an <h1-3>. parseHTMLToStructured must still open a section for
// it (via headingRE, the same heuristic ParseStructured uses for plain-text
// content), rather than dumping the title and body into the preamble.
func TestParseHTMLToStructured_PlainParagraphHeading(t *testing.T) {
	htmlStr := "<p>I – DOS FATOS</p><p>corpo do fato...</p><p>II – DO DIREITO</p><p>corpo do direito...</p>"

	got := parseHTMLToStructured(htmlStr)
	if got == nil {
		t.Fatal("got nil, want non-nil StructuredContent")
	}
	if len(got.Sections) != 2 {
		t.Fatalf("len(Sections) = %d, want 2 (got: %+v)", len(got.Sections), got)
	}

	sec0 := got.Sections[0]
	if sec0.Roman != "I" {
		t.Errorf("Sections[0].Roman = %q, want %q", sec0.Roman, "I")
	}
	if sec0.Title != "DOS FATOS" {
		t.Errorf("Sections[0].Title = %q, want %q", sec0.Title, "DOS FATOS")
	}
	if len(sec0.Paragraphs) != 1 || sec0.Paragraphs[0] != "corpo do fato..." {
		t.Errorf("Sections[0].Paragraphs = %v, want [%q]", sec0.Paragraphs, "corpo do fato...")
	}

	sec1 := got.Sections[1]
	if sec1.Roman != "II" {
		t.Errorf("Sections[1].Roman = %q, want %q", sec1.Roman, "II")
	}
	if sec1.Title != "DO DIREITO" {
		t.Errorf("Sections[1].Title = %q, want %q", sec1.Title, "DO DIREITO")
	}
	if len(sec1.Paragraphs) != 1 || sec1.Paragraphs[0] != "corpo do direito..." {
		t.Errorf("Sections[1].Paragraphs = %v, want [%q]", sec1.Paragraphs, "corpo do direito...")
	}

	if len(got.Preamble.Paragraphs) != 0 {
		t.Errorf("Preamble.Paragraphs = %v, want empty (titles must not leak into preamble)", got.Preamble.Paragraphs)
	}
}

// TestParseHTMLToStructured_H2Heading is a regression test for the
// already-working path: real <h1-3> headings keep opening sections exactly
// as before the plain-paragraph fix.
func TestParseHTMLToStructured_H2Heading(t *testing.T) {
	htmlStr := "<h2>I — DOS FATOS</h2><p>corpo do fato...</p>"

	got := parseHTMLToStructured(htmlStr)
	if got == nil {
		t.Fatal("got nil, want non-nil StructuredContent")
	}
	if len(got.Sections) != 1 {
		t.Fatalf("len(Sections) = %d, want 1 (got: %+v)", len(got.Sections), got)
	}

	sec0 := got.Sections[0]
	if sec0.Roman != "I" {
		t.Errorf("Sections[0].Roman = %q, want %q", sec0.Roman, "I")
	}
	if sec0.Title != "DOS FATOS" {
		t.Errorf("Sections[0].Title = %q, want %q", sec0.Title, "DOS FATOS")
	}
	if len(sec0.Paragraphs) != 1 || sec0.Paragraphs[0] != "corpo do fato..." {
		t.Errorf("Sections[0].Paragraphs = %v, want [%q]", sec0.Paragraphs, "corpo do fato...")
	}
}

// TestParseHTMLToStructured_NoHeading verifies that content with no heading
// at all (neither <h1-3> nor a plain-paragraph roman-numeral title) keeps
// falling entirely into the preamble, with no sections opened.
func TestParseHTMLToStructured_NoHeading(t *testing.T) {
	htmlStr := "<p>Excelentíssimo Senhor Doutor Juiz de Direito.</p><p>Vem, respeitosamente, requerer o que segue.</p>"

	got := parseHTMLToStructured(htmlStr)
	if got == nil {
		t.Fatal("got nil, want non-nil StructuredContent")
	}
	if len(got.Sections) != 0 {
		t.Errorf("len(Sections) = %d, want 0", len(got.Sections))
	}
	want := []string{
		"Excelentíssimo Senhor Doutor Juiz de Direito.",
		"Vem, respeitosamente, requerer o que segue.",
	}
	if len(got.Preamble.Paragraphs) != len(want) {
		t.Fatalf("Preamble.Paragraphs = %v, want %v", got.Preamble.Paragraphs, want)
	}
	for i, p := range want {
		if got.Preamble.Paragraphs[i] != p {
			t.Errorf("Preamble.Paragraphs[%d] = %q, want %q", i, got.Preamble.Paragraphs[i], p)
		}
	}
}

// TestStableSectionID cobre o id estável: romano em minúsculo, fallback por
// posição sem romano OU em colisão, e UNICIDADE garantida (o byID map do
// /iterate colidiria com ids repetidos).
func TestStableSectionID(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	cases := []struct {
		roman string
		ord   int
		want  string
	}{
		{"I", 1, "i"},         // romano → minúsculo
		{"II", 2, "ii"},       // idem
		{"I", 3, "s3"},        // romano repetido → cai na posição (unicidade)
		{"", 4, "s4"},         // sem romano → posição
		{"  iii  ", 5, "iii"}, // trim + lower
	}
	for _, c := range cases {
		if got := stableSectionID(c.roman, c.ord, seen); got != c.want {
			t.Errorf("stableSectionID(%q,%d) = %q, want %q", c.roman, c.ord, got, c.want)
		}
	}
	// Todos os ids gerados devem ser distintos.
	if len(seen) != len(cases) {
		t.Errorf("ids colidiram: seen=%v (esperado %d únicos)", seen, len(cases))
	}
}

// TestParseHTMLToStructured_DuplicateRomanUniqueIDs prova que dois headings com o
// MESMO romano não geram ids repetidos (o segundo cai na posição).
func TestParseHTMLToStructured_DuplicateRomanUniqueIDs(t *testing.T) {
	t.Parallel()
	htmlStr := "<h2>I – DAS PRELIMINARES</h2><p>a</p><h2>I – OUTRA</h2><p>b</p>"
	got := parseHTMLToStructured(htmlStr)
	if got == nil || len(got.Sections) != 2 {
		t.Fatalf("sections = %v", got)
	}
	if got.Sections[0].ID == got.Sections[1].ID {
		t.Errorf("ids repetidos: %q e %q", got.Sections[0].ID, got.Sections[1].ID)
	}
	if got.Sections[0].ID != "i" || got.Sections[1].ID != "s2" {
		t.Errorf("ids = %q,%q, want i,s2", got.Sections[0].ID, got.Sections[1].ID)
	}
}

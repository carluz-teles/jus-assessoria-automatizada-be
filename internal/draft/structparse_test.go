package draft

import (
	"testing"
)

func TestParseStructured_Empty(t *testing.T) {
	t.Parallel()
	if got := ParseStructured(""); got != nil {
		t.Fatalf("empty content → want nil, got %+v", got)
	}
	if got := ParseStructured("   \n\n  \n"); got != nil {
		t.Fatalf("whitespace-only content → want nil, got %+v", got)
	}
}

func TestParseStructured_PreambleOnly(t *testing.T) {
	t.Parallel()
	content := "EXCELENTÍSSIMO SENHOR JUIZ\n\nProcesso nº 0000001-00.2026\n\nPROLHETI, já qualificada..."
	got := ParseStructured(content)
	if got == nil {
		t.Fatal("expected non-nil StructuredContent")
	}
	if len(got.Preamble.Paragraphs) != 3 {
		t.Errorf("preamble paragraphs = %d, want 3", len(got.Preamble.Paragraphs))
	}
	if len(got.Sections) != 0 {
		t.Errorf("sections = %d, want 0 (no headings)", len(got.Sections))
	}
}

func TestParseStructured_FullDefesa(t *testing.T) {
	t.Parallel()
	content := "EXCELENTÍSSIMO SENHOR JUIZ\n" +
		"\n" +
		"Processo nº 0000001-00.2026\n" +
		"\n" +
		"PROLHETI, já qualificada nos autos, vem apresentar DEFESA.\n" +
		"\n" +
		"I — Dos fatos\n" +
		"\n" +
		"1. Trata-se de execução fundada em nota promissória.\n" +
		"\n" +
		"2. A cártula não vem acompanhada de contrato.\n" +
		"\n" +
		"II — Do direito\n" +
		"\n" +
		"3. Nos termos do art. 917 do CPC…\n" +
		"\n" +
		"III — Dos pedidos\n" +
		"\n" +
		"4. Requer a extinção da execução."

	got := ParseStructured(content)
	if got == nil {
		t.Fatal("expected non-nil StructuredContent")
	}
	if len(got.Preamble.Paragraphs) != 3 {
		t.Errorf("preamble = %d, want 3", len(got.Preamble.Paragraphs))
	}
	if len(got.Sections) != 3 {
		t.Fatalf("sections = %d, want 3", len(got.Sections))
	}

	// Section I
	s := got.Sections[0]
	if s.Roman != "I" || s.Title != "Dos fatos" || s.ShortTitle != "fatos" || s.ID != "fatos" {
		t.Errorf("section[0] = %+v, want Roman=I Title='Dos fatos' Short='fatos' ID='fatos'", s)
	}
	if len(s.Paragraphs) != 2 {
		t.Errorf("section[0].paragraphs = %d, want 2", len(s.Paragraphs))
	}

	// Section II
	s = got.Sections[1]
	if s.Roman != "II" || s.Title != "Do direito" || s.ShortTitle != "direito" {
		t.Errorf("section[1] = %+v", s)
	}

	// Section III
	s = got.Sections[2]
	if s.Roman != "III" || s.Title != "Dos pedidos" || s.ShortTitle != "pedidos" {
		t.Errorf("section[2] = %+v", s)
	}
}

func TestParseStructured_DashVariants(t *testing.T) {
	t.Parallel()
	for _, dash := range []string{"—", "–", "-"} {
		content := "Preambulo.\n\nI " + dash + " Dos fatos\n\n1. Fato."
		got := ParseStructured(content)
		if got == nil || len(got.Sections) != 1 {
			t.Errorf("dash %q → sections = %v", dash, got)
			continue
		}
		if got.Sections[0].Roman != "I" || got.Sections[0].Title != "Dos fatos" {
			t.Errorf("dash %q → section = %+v", dash, got.Sections[0])
		}
	}
}

func TestParseStructured_HeadingWithContinuation(t *testing.T) {
	t.Parallel()
	// The LLM sometimes emits the heading + first paragraph in the same "block"
	// (no blank line between them). Parser must recover.
	content := "Preâmbulo.\n\nI — Dos fatos\nEsta linha veio junto.\n\n2. Segundo parágrafo."
	got := ParseStructured(content)
	if got == nil || len(got.Sections) != 1 {
		t.Fatalf("expected 1 section, got %v", got)
	}
	s := got.Sections[0]
	if len(s.Paragraphs) != 2 {
		t.Errorf("paragraphs = %d, want 2 (continuation + second)", len(s.Paragraphs))
	}
	if s.Paragraphs[0] != "Esta linha veio junto." {
		t.Errorf("paragraphs[0] = %q, want 'Esta linha veio junto.'", s.Paragraphs[0])
	}
}

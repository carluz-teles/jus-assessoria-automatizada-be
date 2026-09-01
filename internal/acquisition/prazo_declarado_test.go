package acquisition

import "testing"

// TestExtractPrazoDeclarado proves the deterministic (regex, never LLM) extraction of a
// declared day-count prazo from an intimação's free-text teor, covering the "no prazo
// de N dias" / "prazo de N (por extenso) dias" / "prazo de N dias úteis|corridos"
// families and the miss case (no recognizable prazo mention → "").
func TestExtractPrazoDeclarado(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "no prazo de N dias — real teor example",
			in:   "Intime-se a parte exequente para, no prazo de 5 dias, indicar bens à penhora.",
			want: "5 dias",
		},
		{
			name: "prazo de N (por extenso) dias corridos",
			in:   "Fica a parte intimada, abrindo-se prazo de 15 (quinze) dias corridos para manifestação.",
			want: "15 dias",
		},
		{
			name: "prazo de N dias úteis suffix",
			in:   "Concede-se prazo de 10 dias úteis para apresentação de defesa.",
			want: "10 dias",
		},
		{
			name: "prazo de N dias corridos suffix",
			in:   "Concede-se prazo de 20 dias corridos para cumprimento.",
			want: "20 dias",
		},
		{
			name: "HTML-wrapped teor is stripped before matching",
			in:   "<p>Intime-se a parte, no prazo de 5 dias, indicar bens.</p>",
			want: "5 dias",
		},
		{
			name: "single-digit day count",
			in:   "no prazo de 2 dias",
			want: "2 dias",
		},
		{
			name: "no prazo mention — miss",
			in:   "Ficam as partes cientificadas do despacho proferido nos autos.",
			want: "",
		},
		{
			name: "prazo mentioned without a day count — miss",
			in:   "O prazo será fixado em audiência oportunamente.",
			want: "",
		},
		{
			name: "empty string — miss",
			in:   "",
			want: "",
		},
		{
			name: "unrelated numeric content is not confused for a prazo",
			in:   "Processo nº 1234567-89.2023.8.26.0100 distribuído em 10/05/2023.",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := extractPrazoDeclarado(tt.in)
			if got != tt.want {
				t.Errorf("extractPrazoDeclarado(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

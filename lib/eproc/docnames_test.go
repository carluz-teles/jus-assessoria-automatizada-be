package eproc

import "testing"

func TestDocumentTypeLabel(t *testing.T) {
	tests := []struct {
		name string
		code string
		want string
	}{
		{name: "known code, uppercase", code: "SENT", want: "Sentença"},
		{name: "known code, lowercase + spaces", code: "  sent ", want: "Sentença"},
		{name: "known multi-word code with accent", code: "PLANILHA DE CÁLCULO", want: "Planilha de cálculo"},
		{name: "unknown code returns empty for fallback", code: "INCRESSIS", want: ""},
		{name: "empty is empty", code: "", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DocumentTypeLabel(tt.code); got != tt.want {
				t.Errorf("DocumentTypeLabel(%q) = %q, want %q", tt.code, got, tt.want)
			}
		})
	}
}

func TestHumanizeCode(t *testing.T) {
	tests := []struct {
		name string
		code string
		want string
	}{
		{name: "shouting multi-word preserves accent", code: "PLANILHA DE CÁLCULO", want: "Planilha de cálculo"},
		{name: "single word", code: "INCRESSIS", want: "Incressis"},
		{name: "already lowercase", code: "petição", want: "Petição"},
		{name: "trims surrounding space", code: "  OFICIO  ", want: "Oficio"},
		{name: "empty in, empty out", code: "", want: ""},
		{name: "whitespace only", code: "   ", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := HumanizeCode(tt.code); got != tt.want {
				t.Errorf("HumanizeCode(%q) = %q, want %q", tt.code, got, tt.want)
			}
		})
	}
}

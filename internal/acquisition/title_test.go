package acquisition

import "testing"

// TestBuildCaseTitle covers the 3-tier priority (label > réu+CNJ > classe·assunto) plus
// the defensive empty-string edge cases (write/read paths normalize "" to nil/NULL, but
// the pure function must not surface a blank title if that guard is ever bypassed).
func TestBuildCaseTitle(t *testing.T) {
	t.Parallel()

	label := "Ação de Cobrança — Cliente ACME"
	defendant := "BANCO XYZ S.A."

	tests := []struct {
		name          string
		label         *string
		defendantName *string
		cnjNumber     string
		class         string
		subject       string
		want          string
	}{
		{
			name:          "label wins over réu and classe·assunto",
			label:         &label,
			defendantName: &defendant,
			cnjNumber:     "0000001-11.2024.8.26.0100",
			class:         "Procedimento Comum Cível",
			subject:       "Cobrança",
			want:          "Ação de Cobrança — Cliente ACME",
		},
		{
			name:          "no label, réu captured: defendant + CNJ",
			label:         nil,
			defendantName: &defendant,
			cnjNumber:     "0000001-11.2024.8.26.0100",
			class:         "Procedimento Comum Cível",
			subject:       "Cobrança",
			want:          "BANCO XYZ S.A. · 0000001-11.2024.8.26.0100",
		},
		{
			name:          "no label, no réu: classe·assunto fallback (zero regression)",
			label:         nil,
			defendantName: nil,
			cnjNumber:     "0000001-11.2024.8.26.0100",
			class:         "Procedimento Comum Cível",
			subject:       "Cobrança",
			want:          "Procedimento Comum Cível · Cobrança",
		},
		{
			name:          "empty label (defensive) falls through to réu",
			label:         strPtrTitle(""),
			defendantName: &defendant,
			cnjNumber:     "0000001-11.2024.8.26.0100",
			class:         "Procedimento Comum Cível",
			subject:       "Cobrança",
			want:          "BANCO XYZ S.A. · 0000001-11.2024.8.26.0100",
		},
		{
			name:          "empty defendant name (defensive) falls through to fallback",
			label:         nil,
			defendantName: strPtrTitle(""),
			cnjNumber:     "0000001-11.2024.8.26.0100",
			class:         "Procedimento Comum Cível",
			subject:       "Cobrança",
			want:          "Procedimento Comum Cível · Cobrança",
		},
		{
			name:          "no label, no réu, empty class/subject too: still no panic",
			label:         nil,
			defendantName: nil,
			cnjNumber:     "0000001-11.2024.8.26.0100",
			class:         "",
			subject:       "",
			want:          " · ",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := BuildCaseTitle(tt.label, tt.defendantName, tt.cnjNumber, tt.class, tt.subject)
			if got != tt.want {
				t.Errorf("BuildCaseTitle() = %q, want %q", got, tt.want)
			}
		})
	}
}

// strPtrTitle is a local test helper (avoids colliding with the package's own
// strPtrOrNil, which has different empty-string semantics).
func strPtrTitle(s string) *string { return &s }

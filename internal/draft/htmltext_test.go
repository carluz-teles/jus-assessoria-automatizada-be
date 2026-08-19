package draft

import "testing"

func TestStripHTML(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain text untouched", "Fica intimada a parte.", "Fica intimada a parte."},
		{"empty", "", ""},
		{"simple tags stripped", "<p>Ficam <b>as partes</b> intimadas</p>", "Ficam as partes intimadas"},
		{"entities unescaped", "Jo&atilde;o &amp; Maria &lt;3", "João & Maria <3"},
		{"script/style dropped", "<style>.x{color:red}</style><p>Texto</p><script>alert(1)</script>", "Texto"},
		{
			"block breaks become newlines",
			"<html><body><p>Primeiro</p><p>Segundo</p></body></html>",
			"Primeiro\nSegundo",
		},
		{"whitespace collapsed", "<p>a    b\t\tc</p>", "a b c"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stripHTML(tt.in); got != tt.want {
				t.Errorf("stripHTML(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

package pdfgen

import (
	"strings"
	"testing"
	"time"
)

// TestBuildFullHTML_StripsLLMClosingLine — o LLM (v9 mesmo com prompt
// proibindo) às vezes escreve "Franca/SP, [data]." no fim; buildFullHTML
// remove ANTES de injetar o rodapé fixo, senão o PDF fica com data duplicada.
//
// A verificação é indireta: chama buildFullHTML SEM rodapé fixo (Signer.Name
// vazio) — o strip só remove o parágrafo do LLM e o output final não tem
// mais nenhuma marca do fechamento. Depois valida que o rodapé fixo, quando
// habilitado, aparece como bloco distinto (não confunde com o parágrafo do
// LLM porque este foi removido).
func TestBuildFullHTML_StripsLLMClosingLine(t *testing.T) {
	signedAt := time.Date(2026, 8, 24, 20, 37, 0, 0, time.UTC)
	cases := []struct {
		name string
		html string
		// gone: substrings que NÃO podem sobrar no HTML final SEM rodapé fixo
		// (o strip removeu; rodapé off pra não ambiguar).
		gone []string
	}{
		{
			name: "com marcador [data]",
			html: `<p>Nestes termos, pede deferimento.</p><p>Franca/SP, [data].</p>`,
			gone: []string{"Franca/SP", "[data]"},
		},
		{
			name: "data por extenso",
			html: `<p>Nestes termos, pede deferimento.</p><p>Franca, 24 de agosto de 2026.</p>`,
			gone: []string{`Franca, 24 de agosto de 2026`},
		},
		{
			name: "com atributos de estilo",
			html: `<p style="text-align:right">São Paulo/SP, 15 de outubro de 2025</p>`,
			gone: []string{"São Paulo/SP, 15 de outubro"},
		},
		{
			name: "sem UF, com data extenso",
			html: `<p>Franca, [ data ].</p>`,
			gone: []string{"Franca, "},
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			// Signer sem Name → sem rodapé fixo → strip é o único autor de saída.
			out := buildFullHTML(RenderHTMLInput{
				HTML:     tt.html,
				SignedAt: signedAt,
				Signer:   Signer{},
			})
			for _, g := range tt.gone {
				if strings.Contains(out, g) {
					t.Errorf("output contém %q — deveria ter sido strippado.\nHTML: %s", g, out)
				}
			}
		})
	}
}

// TestBuildFullHTML_FixedFooter — quando Signer.Name está presente, o rodapé
// fixo é injetado com place + data + nome (o strip do LLM já rodou antes).
func TestBuildFullHTML_FixedFooter(t *testing.T) {
	signedAt := time.Date(2026, 8, 24, 20, 37, 0, 0, time.UTC)
	out := buildFullHTML(RenderHTMLInput{
		HTML:     `<p>Nestes termos, pede deferimento.</p>`,
		SignedAt: signedAt,
		Signer:   Signer{Name: "Fulano de Tal", Place: "Franca", OAB: "12345/SP"},
	})
	for _, want := range []string{"Fulano de Tal", "24 de agosto de 2026", "Franca", "OAB 12345/SP"} {
		if !strings.Contains(out, want) {
			t.Errorf("rodapé fixo não contém %q", want)
		}
	}
}

// TestBuildFullHTML_PreservesUnrelatedContent — o strip não pode apagar
// parágrafos que só parecem uma linha de fechamento mas são texto útil.
func TestBuildFullHTML_PreservesUnrelatedContent(t *testing.T) {
	signedAt := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	preserve := []string{
		`<p>Trata-se de execução extrajudicial.</p>`,
		// Parágrafo longo — não deve casar (mais de 50 chars entre "Cidade" e ",")
		`<p>Trata-se de ato ordinatório do juízo da comarca, cuja distribuição por lei se dá de forma automática, 24 de agosto de 2026.</p>`,
	}
	for _, p := range preserve {
		out := buildFullHTML(RenderHTMLInput{
			HTML:     p,
			SignedAt: signedAt,
			Signer:   Signer{Name: "Fulano"},
		})
		if !strings.Contains(out, p[3:len(p)-4]) { // conteúdo interno do <p>
			t.Errorf("parágrafo útil foi removido por engano:\n%s", p)
		}
	}
}

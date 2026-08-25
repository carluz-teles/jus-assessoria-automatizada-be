// Package pdfgen — Fase C do editor rico: renderer HTML→PDF via chromedp.
//
// A partir da Fase B, o editor Tiptap persiste HTML rico em draft.content_html
// e formatação inline (bold/italic/color/tabelas) precisa fluir intacta até o
// PDF final. maroto/gofpdf (fonte core PDF, sem HTML) não dá conta desse
// nível de fidelidade — trocamos por Chromium headless via chromedp, que
// interpreta HTML/CSS nativamente e produz PDF pixel-perfect.
//
// Trade-offs assumidos:
//   - +Chromium no container (build stage já embutido no Dockerfile)
//   - Determinismo: Chromium é determinístico se rodar com mesma versão +
//     mesmo HTML + mesma viewport + mesma data. Como o Sign gera o PDF
//     imediatamente antes de assinar, sempre é o mesmo run — o digest é
//     estável no momento da assinatura.
//   - Font: usamos a mesma Liberation Serif embutida no binário
//     (fontRegularBytes/fontBoldBytes de pdfgen.go). O HTML injetado tem
//     @font-face inline pra usar essa fonte, garantindo bate visualmente
//     com o WYSIWYG do Tiptap (ambos usam mesma familia + métricas).
//
// A função RenderHTML recebe o HTML "cru" do editor + Signer (nome/OAB do
// cert) + timestamps de contexto. Ela envolve o HTML em uma página completa
// (HTML doctype + @page A4 + margens forenses + CSS de tabela + bloco de
// assinatura no fim) e chama chromedp pra imprimir em PDF.

package pdfgen

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// RenderHTMLInput carrega tudo que o renderer HTML→PDF precisa. Instâncias
// separadas do renderer entre requests — ChromeDP maneja o pool interno.
type RenderHTMLInput struct {
	// HTML é o conteúdo bruto do editor (Tiptap). Deve incluir <p>/<h2>/etc
	// no top-level, sem <html>/<head>/<body> — a fn envolve. Nunca nil.
	HTML string
	// Signer é o titular do cert usado; se Name != "", vira bloco visual
	// centralizado no fim (linha + Nome + OAB). Vazio = sem bloco.
	Signer Signer
	// SignedAt é usado pra substituir [data] textual dentro do HTML antes de
	// renderizar (mesma semântica de fillPlaceholders do maroto path).
	SignedAt time.Time
}

// dateRegex — [data] com espaços opcionais, case-insensitive (mesmo padrão
// de placeholderDateRe do domain.go, duplicado aqui pra pdfgen não depender
// do internal/draft).
var dateRegexHTML = regexp.MustCompile(`(?i)\[\s*data\s*\]`)

// closingLineRegex captura o parágrafo de fechamento que o LLM às vezes
// escreve mesmo com o prompt v9 proibindo. Casa qualquer <p> (com ou sem
// atributos) cujo conteúdo seja "Cidade[/UF], [data]" OU "Cidade[/UF], data
// por extenso". O rodapé fixo (buildFullHTML → sigBlock) já injeta lugar+data
// autoritativos do processo/assinatura, então este parágrafo VAI DUPLICAR se
// não removido. Escopo cirúrgico: só parágrafos curtos que casam o padrão
// exato (evita apagar texto útil por engano).
//
// Formatos cobertos:
//
//	<p>Franca/SP, [data].</p>
//	<p>Franca, 24 de agosto de 2026.</p>
//	<p>São Paulo/SP, 15 de outubro de 2025</p>
//	<p style="text-align:right">Rio de Janeiro/RJ, [ data ].</p>
var closingLineRegex = regexp.MustCompile(
	`(?is)<p\b[^>]*>\s*[A-ZÁÉÍÓÚÂÊÎÔÛÃÕÇ][A-Za-zÀ-ÿ\s\.]{1,50}(?:/[A-Z]{2})?\s*,\s*(?:\[\s*data\s*\]|\d{1,2}\s+de\s+[a-zç]+\s+de\s+\d{4})\.?\s*</p>`,
)

// monthsPT é a tabela de meses em português usada pra formatar [data].
var monthsPT = [...]string{
	"janeiro", "fevereiro", "março", "abril", "maio", "junho",
	"julho", "agosto", "setembro", "outubro", "novembro", "dezembro",
}

// RenderHTML gera bytes de PDF a partir do HTML do editor rico. Roda uma
// instância Chromium headless via chromedp e usa Page.printToPDF pra
// serializar a folha. O ctx do chamador governa timeout — o wrapper
// externo (domain.Sign) impõe seu próprio bound via signOnceBounded.
func RenderHTML(ctx context.Context, in RenderHTMLInput) ([]byte, error) {
	fullHTML := buildFullHTML(in)
	dataURL := "data:text/html;base64," + base64.StdEncoding.EncodeToString([]byte(fullHTML))

	// Alocador headless — usa Chromium do PATH (instalado no Dockerfile
	// na Fase C). Em dev local ou onde só existe google-chrome, resolveExecPath()
	// escolhe o binário certo.
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", true),
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("hide-scrollbars", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("run-all-compositor-stages-before-draw", true),
	)
	if path := resolveChromePath(); path != "" {
		opts = append(opts, chromedp.ExecPath(path))
	}
	allocCtx, allocCancel := chromedp.NewExecAllocator(ctx, opts...)
	defer allocCancel()

	browserCtx, browserCancel := chromedp.NewContext(allocCtx)
	defer browserCancel()

	var pdf []byte
	err := chromedp.Run(browserCtx,
		chromedp.Navigate(dataURL),
		chromedp.ActionFunc(func(c context.Context) error {
			var err error
			// A4 = 8.27 x 11.69 in; margens ABNT 3-3-2-2 cm ~ 1.18-1.18-0.79-0.79 in.
			pdf, _, err = page.PrintToPDF().
				WithPrintBackground(false).
				WithPaperWidth(8.27).
				WithPaperHeight(11.69).
				WithMarginTop(1.18).
				WithMarginBottom(0.79).
				WithMarginLeft(1.18).
				WithMarginRight(0.79).
				WithDisplayHeaderFooter(true).
				WithHeaderTemplate(`<div></div>`).
				WithFooterTemplate(`<div style="font-family: 'Liberation Serif', 'Times New Roman', serif; font-size: 8pt; width: 100%; text-align: center; color: #666;"><span class="pageNumber"></span> / <span class="totalPages"></span></div>`).
				Do(c)
			return err
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("chromedp printToPDF: %w", err)
	}
	return pdf, nil
}

// buildFullHTML envolve o content bruto do editor numa página completa com
// CSS forense embutido + bloco de assinatura opcional. Substitui [data] pela
// data por extenso em pt-BR (mesma lógica do fillPlaceholders do maroto path).
func buildFullHTML(in RenderHTMLInput) string {
	dateStr := fmt.Sprintf("%d de %s de %d",
		in.SignedAt.Day(), monthsPT[in.SignedAt.Month()-1], in.SignedAt.Year())
	// Strip da linha de fechamento gerada pelo LLM (Débito #1). Rodapé fixo
	// injetado abaixo (sigBlock) é a fonte autoritativa — sem strip a data
	// aparece 2x no PDF. Vem antes do dateRegex pra remover mesmo o formato
	// com marcador [data] literal.
	content := closingLineRegex.ReplaceAllString(in.HTML, "")
	content = dateRegexHTML.ReplaceAllString(content, dateStr)

	// Bloco de fechamento fixo — lugar + data + assinatura. NÃO gerado pelo
	// LLM (prompt draft_minuta v9 proíbe explicitamente): é injetado aqui,
	// centralizado, com dados AUTORITATIVOS do processo (judging_body) e
	// do certificado usado na assinatura. Evita:
	//   • LLM inventar cidade/OAB errados
	//   • Data da minuta divergir da data do protocolo
	//   • Rodapé genérico que precisa ser reescrito ao trocar certificado
	sigBlock := ""
	if in.Signer.Name != "" {
		placeDateLine := ""
		if in.Signer.Place != "" {
			placeDateLine = fmt.Sprintf(
				`<div style="text-align:right; margin-top:40px">%s, %s.</div>`,
				htmlEscape(in.Signer.Place), dateStr,
			)
		} else {
			// Fallback: sem cidade parseada, ainda mostra a data no bloco.
			placeDateLine = fmt.Sprintf(
				`<div style="text-align:right; margin-top:40px">%s.</div>`,
				dateStr,
			)
		}
		oabLine := ""
		if in.Signer.OAB != "" {
			oabLine = fmt.Sprintf(`<div style="text-align:center">OAB %s</div>`, htmlEscape(in.Signer.OAB))
		}
		sigBlock = fmt.Sprintf(`
			%s
			<div style="margin-top:60px; text-align:center; page-break-inside:avoid">
				<div style="border-top:1px solid #000; width:40%%; margin:0 auto 4px auto"></div>
				<div style="font-weight:bold">%s</div>
				%s
			</div>`, placeDateLine, htmlEscape(in.Signer.Name), oabLine)
	}

	// @font-face embute a Liberation Serif via data:URL — bate 1:1 com o
	// editor Tiptap (mesma família) e não depende de fonte do sistema.
	regularB64 := base64.StdEncoding.EncodeToString(fontRegularBytes)
	boldB64 := base64.StdEncoding.EncodeToString(fontBoldBytes)

	return fmt.Sprintf(`<!doctype html>
<html lang="pt-BR">
<head>
<meta charset="utf-8">
<title>Peça</title>
<style>
@font-face { font-family: "Liberation Serif"; font-weight: 400; src: url(data:font/ttf;base64,%s) format("truetype"); }
@font-face { font-family: "Liberation Serif"; font-weight: 700; src: url(data:font/ttf;base64,%s) format("truetype"); }
* { box-sizing: border-box; }
html, body {
  font-family: "Liberation Serif", "Times New Roman", Times, serif;
  font-size: 12pt;
  line-height: 1.5;
  color: #000;
  margin: 0;
  padding: 0;
}
p { margin: 0 0 0.5em 0; text-align: justify; hyphens: auto; }
h1 { font-size: 14pt; text-transform: uppercase; text-align: center; font-weight: 700; margin: 1.5em 0 0.75em 0; }
h2 { font-size: 12pt; text-transform: uppercase; text-align: center; font-weight: 700; margin: 1.5em 0 0.75em 0; }
h3 { font-size: 12pt; text-align: center; font-weight: 700; margin: 1.5em 0 0.75em 0; }
ul, ol { padding-left: 1.5em; margin: 0.5em 0; }
li p { margin: 0; }
blockquote { border-left: 3px solid #999; margin: 0.75em 0 0.75em 2em; padding-left: 1em; color: #333; font-size: 10pt; line-height: 1.2; }
hr { border: none; border-top: 1px solid #999; margin: 1em 0; }
table { border-collapse: collapse; margin: 1em 0; width: 100%%; table-layout: fixed; }
td, th { border: 1px solid #000; padding: 6px 8px; vertical-align: top; }
th { background: #eee; font-weight: 700; text-align: left; }
</style>
</head>
<body>
%s
%s
</body>
</html>`, regularB64, boldB64, content, sigBlock)
}

// resolveChromePath procura um binário Chromium/Chrome no PATH, na ordem:
//  1. env CHROME_BIN (override explícito, útil pra CI/dev)
//  2. chromium (Dockerfile runtime-chromium; Alpine/Debian)
//  3. chromium-browser (Debian antigo)
//  4. google-chrome / google-chrome-stable (dev local, macOS/Ubuntu desktop)
// Retorna "" se nenhum encontrado — chromedp cai no default (que pode falhar
// em ambientes minimalistas, mas o log de erro será claro).
func resolveChromePath() string {
	if p := os.Getenv("CHROME_BIN"); p != "" {
		return p
	}
	for _, name := range []string{"chromium", "chromium-browser", "google-chrome", "google-chrome-stable"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

func htmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, `"`, "&quot;")
	return s
}

package draft

import (
	"bytes"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer/html"
)

// markdown.go — conversão markdown → HTML no fim do streaming da geração
// (draft_minuta v8+). O LLM emite markdown (CommonMark + GFM tables) porque
// é robusto ao streaming char-a-char; o BE consolida em HTML aqui antes de
// persistir em `draft.content_html`, mantendo o contrato do editor Tiptap
// e do renderer PDF (chromedp) inalterado.
//
// Extensões habilitadas:
//   - GFM tables (`| col | col |`) — impugnação de valor da causa
//   - GFM strikethrough (`~~texto~~`) — raro em peças, ativado por consistência com GFM
//   - Unsafe HTML rendering — o markdown pode conter <br> ou <div>, mas o output final
//     ainda passa pelo Tiptap (que sanitiza). Sem "Unsafe" o goldmark escaparia HTML
//     bruto que o LLM eventualmente inclua, quebrando compatibilidade com iterate legacy.
//   - Hard wraps DESLIGADO — quebras de linha soltas dentro de um parágrafo NÃO viram
//     <br>, o comportamento é o do CommonMark (parágrafo por linha em branco).
var mdConverter = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithParserOptions(parser.WithAutoHeadingID()),
	goldmark.WithRendererOptions(html.WithUnsafe()),
)

// markdownToHTML converts the LLM's `draft_markdown` field to the HTML the
// editor and PDF renderer expect. Empty input returns empty output — the
// caller decides whether that's a failure (schema strict already guards).
func markdownToHTML(md string) (string, error) {
	if md == "" {
		return "", nil
	}
	var buf bytes.Buffer
	if err := mdConverter.Convert([]byte(md), &buf); err != nil {
		return "", err
	}
	return buf.String(), nil
}

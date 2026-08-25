// Package pdfgen renders a Draft's structured content into a PDF byte slice
// using maroto/v2 seguindo os padrões de formatação forense brasileira:
//
//   - Fonte: Liberation Serif 12 (métricas idênticas a Times New Roman; OFL);
//     Times New Roman é a fonte forense majoritária (Res. CNJ 65/2008), e a
//     Liberation Serif é o substituto Unicode-safe (as fontes core do PDF
//     — Times/Helvetica/Courier — só entendem CP1252, mojibake em UTF-8).
//   - Margens: esquerda 3cm, superior 3cm, direita 2cm, inferior 2cm (ABNT)
//   - Endereçamento: CAIXA ALTA · negrito · centralizado + 8 linhas em branco
//   - Corpo: justificado; nunca justificar linhas isoladas (título, assinatura)
//   - Seções (I — DOS FATOS, ...): CAIXA ALTA · negrito · centralizado; 2 linhas antes/depois
//   - Fecho ("Nestes termos..."): alinhado à esquerda, sem indent
//   - Local/data: alinhado à direita
//   - Assinatura: linha visual + Nome + OAB centralizados no fim (usa Signer)
//
// Determinístico: mesma Draft → mesmos bytes → mesmo hash. Sem isso, o digest
// que a gente assina em PAdES não bate com o PDF em disco.
package pdfgen

import (
	_ "embed"

	"github.com/johnfercher/maroto/v2"
	"github.com/johnfercher/maroto/v2/pkg/components/line"
	"github.com/johnfercher/maroto/v2/pkg/components/text"
	"github.com/johnfercher/maroto/v2/pkg/config"
	"github.com/johnfercher/maroto/v2/pkg/consts/align"
	"github.com/johnfercher/maroto/v2/pkg/consts/fontstyle"
	"github.com/johnfercher/maroto/v2/pkg/consts/orientation"
	"github.com/johnfercher/maroto/v2/pkg/consts/pagesize"
	"github.com/johnfercher/maroto/v2/pkg/core/entity"
	"github.com/johnfercher/maroto/v2/pkg/props"
)

// Liberation Serif = fonte OFL cujas MÉTRICAS são idênticas às do Times New
// Roman (mesmos avanços por glyph → substituição visual perfeita) mas com
// suporte Unicode completo (UTF-8). As fontes core do PDF ("Times") só
// entendem WinAnsi/CP1252 e recebem UTF-8 como CP1252 → mojibake em ã, ç,
// —, §, etc. Embedar Regular+Bold custa ~750KB no binário; sem ele, o PDF
// jurídico brasileiro fica ilegível. Font source:
// https://github.com/liberationfonts/liberation-fonts (SIL Open Font License).
var (
	//go:embed fonts/LiberationSerif-Regular.ttf
	fontRegularBytes []byte
	//go:embed fonts/LiberationSerif-Bold.ttf
	fontBoldBytes []byte
)

const fontLiberationSerif = "LiberationSerif"

// embeddedFont satisfies maroto's entity.CustomFont interface — bytes carrega
// o TTF (registrado via gofpdf.AddUTF8FontFromBytes) e File fica vazio.
type embeddedFont struct {
	family string
	style  fontstyle.Type
	bytes  []byte
}

func (f embeddedFont) GetFamily() string        { return f.family }
func (f embeddedFont) GetStyle() fontstyle.Type { return f.style }
func (f embeddedFont) GetFile() string          { return "" }
func (f embeddedFont) GetBytes() []byte         { return f.bytes }

// Draft é o input do renderer (espelha internal/draft.DraftDetailView, mantido
// separado pra pdfgen não depender do domain).
type Draft struct {
	Title    string    // "Petição", "Contestação", "Recurso"…
	CNJ      string    // número do processo (mostrado em page-number override, futuro)
	Preamble []string  // parágrafos de endereçamento + qualificação
	Sections []Section // corpo da peça (Fatos, Direito, Pedidos)
	Signer   Signer    // titular do cert usado — vira bloco de assinatura no fim
}

// Section é um bloco numerado (I — DOS FATOS, II — DO DIREITO, ...).
type Section struct {
	Roman      string
	Title      string
	Paragraphs []string
}

// Signer identifica o titular do cert que assinou a peça. Se Name vazio,
// o bloco de assinatura não é impresso (usado só quando o Sign roda).
//
// Place é a cidade do foro, extraída do court_record.judging_body pelo
// domain.Sign (parseCityFromJudgingBody). Vai no bloco de fechamento como
// "[Place], [data por extenso]." — a data é a SignedAt do render. Vazio =
// omite a linha lugar/data (fallback defensivo; o BE tenta sempre extrair).
type Signer struct {
	Name  string // CN do cert ("CARLOS TELES TESTE")
	OAB   string // "347019/SP" — pode vir vazio se o cert não tiver
	Place string // Cidade do foro ("Franca") — parseada do judging_body
}

// Medidas em mm (padrão do maroto). 1cm = 10mm.
const (
	marginLeft   = 30.0 // 3cm
	marginTop    = 30.0
	marginRight  = 20.0 // 2cm
	marginBottom = 20.0

	bodySize    = 12.0 // Res. CNJ 65: 10-12pt
	titleSize   = 12.0 // endereçamento em caixa alta é do mesmo tamanho, só bold
	sectionSize = 12.0 // seções: mesmo tamanho, distingue por bold+caps

	blankLineH = 6.0 // altura de uma "linha em branco" (~ leading do body)
	rowH       = 6.0
)

// Render gera os bytes do PDF. Erros bubble-up do maroto (infra).
func Render(d Draft) ([]byte, error) {
	cfg := config.NewBuilder().
		WithPageSize(pagesize.A4).
		WithOrientation(orientation.Vertical).
		WithLeftMargin(marginLeft).
		WithRightMargin(marginRight).
		WithTopMargin(marginTop).
		WithBottomMargin(marginBottom).
		WithCustomFonts([]entity.CustomFont{
			embeddedFont{family: fontLiberationSerif, style: fontstyle.Normal, bytes: fontRegularBytes},
			embeddedFont{family: fontLiberationSerif, style: fontstyle.Bold, bytes: fontBoldBytes},
		}).
		WithDefaultFont(&props.Font{
			Family: fontLiberationSerif,
			Size:   bodySize,
		}).
		WithPageNumber().
		WithCompression(true).
		Build()

	m := maroto.New(cfg)

	// PREÂMBULO — primeiro parágrafo é endereçamento (CAIXA ALTA+bold+centro),
	// demais são qualificação da parte (justificado). Depois do endereçamento:
	// 8 linhas em branco (padrão forense — separa o "cabeçalho" do corpo).
	for i, p := range d.Preamble {
		if p == "" {
			continue
		}
		if i == 0 {
			m.AddAutoRow(text.NewCol(12, p, props.Text{
				Family: fontLiberationSerif,
				Size:   titleSize,
				Style:  fontstyle.Bold,
				Align:  align.Center,
			}))
			// 8 linhas em branco após endereçamento.
			m.AddRow(blankLineH * 8)
			continue
		}
		m.AddAutoRow(text.NewCol(12, p, props.Text{
			Family: fontLiberationSerif,
			Size:   bodySize,
			Align:  align.Justify,
		}))
		m.AddRow(blankLineH)
	}

	// SEÇÕES — cabeçalho CAIXA ALTA + bold + centralizado, com 2 linhas em
	// branco antes/depois. Parágrafos justificados, separados por 1 linha.
	for _, s := range d.Sections {
		m.AddRow(blankLineH * 2)
		heading := s.Roman
		if s.Title != "" {
			heading = s.Roman + " — " + s.Title
		}
		m.AddAutoRow(text.NewCol(12, heading, props.Text{
			Family: fontLiberationSerif,
			Size:   sectionSize,
			Style:  fontstyle.Bold,
			Align:  align.Center,
		}))
		m.AddRow(blankLineH * 2)
		for _, p := range s.Paragraphs {
			if p == "" {
				continue
			}
			m.AddAutoRow(text.NewCol(12, p, props.Text{
				Family: fontLiberationSerif,
				Size:   bodySize,
				Align:  align.Justify,
			}))
			m.AddRow(blankLineH)
		}
	}

	// BLOCO DE ASSINATURA — só se Signer.Name veio. Vem depois de tudo
	// (idealmente após "Nestes termos, pede deferimento" + "Local, data" que
	// o IA já gerou nos últimos parágrafos da última seção). 5 linhas em
	// branco → linha visual (linha horizontal via component `line`) → nome
	// centralizado → OAB centralizada. Cada um em row separado.
	if d.Signer.Name != "" {
		m.AddRow(blankLineH * 5)
		m.AddRow(rowH, line.NewCol(12, props.Line{
			Thickness:     0.4,
			SizePercent:   40, // linha ocupa 40% da largura da coluna → centralizada visualmente
			Style:         "solid",
			OffsetPercent: 100,
		}))
		m.AddAutoRow(text.NewCol(12, d.Signer.Name, props.Text{
			Family: fontLiberationSerif,
			Size:   bodySize,
			Style:  fontstyle.Bold,
			Align:  align.Center,
		}))
		if d.Signer.OAB != "" {
			m.AddAutoRow(text.NewCol(12, "OAB "+d.Signer.OAB, props.Text{
				Family: fontLiberationSerif,
				Size:   bodySize,
				Align:  align.Center,
			}))
		}
	}

	doc, err := m.Generate()
	if err != nil {
		return nil, err
	}
	return doc.GetBytes(), nil
}

// Package pdfgen renders a Draft's structured content into a PDF byte slice
// using maroto/v2. Deterministic: same Draft → same bytes → same hash. That
// property matters because the digest is what gets signed — any run-to-run
// variance would produce a signature that doesn't match the on-disk PDF.
//
// The layout is intentionally sober (títulos centralizados, seções em block,
// parágrafos justificados) — this is a legal document, not a marketing piece.
// Fatia 2b: enough to pass through the assinatura + protocolo flow. Header
// personalizado (logo do escritório, rodapé com OAB) fica pra fatia futura.
package pdfgen

import (
	"github.com/johnfercher/maroto/v2"
	"github.com/johnfercher/maroto/v2/pkg/components/text"
	"github.com/johnfercher/maroto/v2/pkg/config"
	"github.com/johnfercher/maroto/v2/pkg/consts/align"
	"github.com/johnfercher/maroto/v2/pkg/consts/fontstyle"
	"github.com/johnfercher/maroto/v2/pkg/consts/orientation"
	"github.com/johnfercher/maroto/v2/pkg/consts/pagesize"
	"github.com/johnfercher/maroto/v2/pkg/props"
)

// Draft is the input shape (mirror of internal/draft.DraftDetailView, kept
// separate so pdfgen doesn't depend on the domain package).
type Draft struct {
	Title    string    // "Petição", "Defesa", …
	CNJ      string    // número do processo (mostrado no rodapé)
	Preamble []string  // parágrafos de endereçamento + qualificação
	Sections []Section // corpo da peça
}

// Section is a numbered block (I — DOS FATOS, II — DO DIREITO, …).
type Section struct {
	Roman      string   // "I", "II", "III"
	Title      string   // "DOS FATOS"
	Paragraphs []string // corpo
}

const (
	pageMargin   = 20.0 // mm
	titleSize    = 14.0
	sectionSize  = 12.0
	bodySize     = 11.0
	titleRowH    = 12.0
	sectionRowH  = 10.0
	paragraphGap = 4.0 // extra vertical spacing entre parágrafos (via AutoRow height)
)

// Render generates the PDF bytes. Errors bubble up from maroto — typically infra.
func Render(d Draft) ([]byte, error) {
	cfg := config.NewBuilder().
		WithPageSize(pagesize.A4).
		WithOrientation(orientation.Vertical).
		WithLeftMargin(pageMargin).
		WithRightMargin(pageMargin).
		WithTopMargin(pageMargin).
		WithBottomMargin(pageMargin).
		WithPageNumber().
		WithCompression(true).
		Build()

	m := maroto.New(cfg)

	// Título (uma linha, centralizado, negrito).
	m.AddRow(titleRowH, text.NewCol(12, d.Title, props.Text{
		Size:  titleSize,
		Style: fontstyle.Bold,
		Align: align.Center,
	}))

	// Preâmbulo — cada parágrafo numa AutoRow (altura calculada pelo texto).
	for _, p := range d.Preamble {
		if p == "" {
			continue
		}
		m.AddAutoRow(text.NewCol(12, p, props.Text{
			Size:  bodySize,
			Align: align.Justify,
			Top:   paragraphGap,
		}))
	}

	// Seções — cabeçalho negrito + parágrafos justificados.
	for _, s := range d.Sections {
		heading := s.Roman
		if s.Title != "" {
			heading = s.Roman + " — " + s.Title
		}
		m.AddRow(sectionRowH, text.NewCol(12, heading, props.Text{
			Size:  sectionSize,
			Style: fontstyle.Bold,
			Top:   paragraphGap * 2,
		}))
		for _, p := range s.Paragraphs {
			if p == "" {
				continue
			}
			m.AddAutoRow(text.NewCol(12, p, props.Text{
				Size:  bodySize,
				Align: align.Justify,
				Top:   paragraphGap,
			}))
		}
	}

	doc, err := m.Generate()
	if err != nil {
		return nil, err
	}
	return doc.GetBytes(), nil
}

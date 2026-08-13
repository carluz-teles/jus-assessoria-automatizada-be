package extraction

import (
	"bytes"
	"context"
	"strings"

	"github.com/ledongthuc/pdf"

	"github.com/jusassessoria/platform/lib/apperr"
)

// textLayerVersion identifies the pure-Go PDF text-layer adapter in extractor_version. A
// bump (a library change that alters extraction) makes a re-extraction auditable.
const textLayerVersion = "pdftext-v1"

// minCharsPerPage is the density floor: the AVERAGE non-whitespace characters per page below
// which a PDF is judged to have NO usable text layer (a scan) and Extract reports
// hasTextLayer=false so the dispatcher routes to OCR. Density, not a total-chars floor,
// because a scanned court PDF often carries a thin PJe text-layer header stamp on every page
// (~30–90 chars/page); a 71-page scan then totals ~2100 chars and defeats any fixed total
// floor while carrying zero body text. A real text page has hundreds–thousands of chars, so
// the average cleanly separates a stamped scan (below the floor → OCR) from a text document
// (above it → keep the text layer).
const minCharsPerPage = 100

// TextLayerExtractor is the Fatia 5 adapter: it reads embedded text per page from a PDF's
// text layer using the pure-Go ledongthuc/pdf reader (no cgo, no external binary). If the
// total extracted text is negligible (a scanned PDF), it returns hasTextLayer=false so the
// caller falls back to OCR. It is stateless.
type TextLayerExtractor struct{}

// NewTextLayerExtractor returns the text-layer adapter. Stateless; the worker injects the
// zero value.
func NewTextLayerExtractor() *TextLayerExtractor { return &TextLayerExtractor{} }

var _ TextExtractor = (*TextLayerExtractor)(nil)

// Extract reads per-page text from the PDF's text layer. It opens the bytes as an
// io.ReaderAt (no temp file), iterates pages 1..NumPage, and collects each page's plain
// text. A page with no Contents (a blank/scan page) yields an empty PageText — the page
// count still reflects the real document. hasTextLayer is true only when the average
// non-whitespace chars per page clears minCharsPerPage (see hasUsableTextLayer); otherwise
// the document is a scan (or a scan with a thin per-page stamp) and the caller routes to OCR.
// A malformed PDF the reader cannot open is a terminal invalid (a retry re-reads the same
// bytes).
func (*TextLayerExtractor) Extract(_ context.Context, data []byte) ([]PageText, bool, string, error) {
	reader, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, false, textLayerVersion, apperr.NewInvalid("extraction: open pdf: " + err.Error())
	}

	numPages := reader.NumPage()
	pages := make([]PageText, 0, numPages)
	total := 0
	for i := 1; i <= numPages; i++ {
		p := reader.Page(i)
		text := pagePlainText(p)
		total += len(strings.TrimSpace(text))
		pages = append(pages, PageText{Page: i, Text: text})
	}

	return pages, hasUsableTextLayer(total, numPages), textLayerVersion, nil
}

// hasUsableTextLayer decides — from the total non-whitespace chars and the page count alone —
// whether a PDF's text layer is real or just a scan's stamp. It is a pure helper so the density
// rule is unit-testable without a PDF fixture. hasTextLayer is true only when the document has
// at least one page AND the average chars per page clears minCharsPerPage: a stamped 71-page
// scan (~66 chars/page) falls below and routes to OCR, while a dense text document (hundreds–
// thousands of chars/page) stays above and keeps its text layer. A 0-page document is never
// "usable" (guards the division).
func hasUsableTextLayer(total, numPages int) bool {
	return numPages > 0 && total/numPages >= minCharsPerPage
}

// pagePlainText renders one page's text layer as plain text, row by row (GetTextByRow
// preserves reading order: words within a row joined by spaces, rows by newlines). A null
// page or a page with no Contents (a scan) yields "". Errors from a single page are swallowed
// to "" rather than failing the whole document — a partial extraction still routes to OCR via
// the density floor if it comes up empty.
func pagePlainText(p pdf.Page) string {
	if p.V.IsNull() || p.V.Key("Contents").Kind() == pdf.Null {
		return ""
	}
	rows, err := p.GetTextByRow()
	if err != nil {
		return ""
	}
	var b strings.Builder
	for _, row := range rows {
		for i, word := range row.Content {
			if i > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(word.S)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

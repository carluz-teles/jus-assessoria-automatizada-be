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

// minTextLayerChars is the floor of total extracted characters below which a PDF is judged
// to have NO usable text layer (a scan). Below it, Extract reports hasTextLayer=false so the
// dispatcher routes to OCR. A handful of stray characters (page numbers baked into a scan's
// OCR-less overlay) must not defeat the OCR fallback, hence a non-zero floor rather than "> 0".
const minTextLayerChars = 16

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
// count still reflects the real document. hasTextLayer is true only when the total
// non-whitespace text clears minTextLayerChars; otherwise the document is a scan and the
// caller routes to OCR. A malformed PDF the reader cannot open is a terminal invalid (a
// retry re-reads the same bytes).
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

	return pages, total >= minTextLayerChars, textLayerVersion, nil
}

// pagePlainText renders one page's text layer as plain text, row by row (GetTextByRow
// preserves reading order: words within a row joined by spaces, rows by newlines). A null
// page or a page with no Contents (a scan) yields "". Errors from a single page are swallowed
// to "" rather than failing the whole document — a partial extraction still routes to OCR via
// the total-chars floor if it comes up empty.
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

package extraction

import "context"

// DispatchExtractor is the composite TextExtractor: it tries the text-layer adapter first
// (Fatia 5, cheap + pure-Go) and, when that reports no usable text layer (a scanned PDF),
// falls back to the OCR adapter (Fatia 6, Claude vision). This is the extractor the use case
// actually holds; the two leaf adapters stay single-responsibility behind the same port.
type DispatchExtractor struct {
	textLayer TextExtractor
	ocr       TextExtractor
	html      TextExtractor // optional: handles bytes that sniff as HTML (nil → PDF-only)
}

// NewDispatchExtractor wires the dispatcher to the two PDF leaf adapters. Both are injected as
// the TextExtractor port so the worker composes concrete adapters and tests substitute
// fakes. Use WithHTMLExtractor to also route HTML bytes.
func NewDispatchExtractor(textLayer, ocr TextExtractor) *DispatchExtractor {
	return &DispatchExtractor{textLayer: textLayer, ocr: ocr}
}

// WithHTMLExtractor adds an HTML adapter: bytes that sniff as HTML (see looksLikeHTML) route
// to it instead of the PDF reader, so eproc's HTML documents (and access/error pages served
// under a "pdf" mime) get their text extracted instead of failing as "not a PDF".
func (d *DispatchExtractor) WithHTMLExtractor(html TextExtractor) *DispatchExtractor {
	d.html = html
	return d
}

var _ TextExtractor = (*DispatchExtractor)(nil)

// Extract routes HTML bytes to the HTML adapter first (content-sniffed, since the stored mime
// can't be trusted); otherwise it runs the text-layer adapter — if that found a real text
// layer, that result wins (version = "pdftext-v2"), else it falls back to OCR (a scan).
//
// A text-layer OPEN error also falls back to OCR: the pure-Go reader rejects some VALID PDFs
// (2.0 / advanced 1.7 with cross-reference or object streams) that OCR's poppler rasterizer
// (pdftoppm) handles fine — so those get OCR'd instead of failing terminally. If OCR ALSO can't
// render the bytes (genuinely corrupt), its own (retryable) error surfaces; that a corrupt PDF
// now retries-then-archives rather than failing immediately is acceptable, since OCR defaults to
// the free local Tesseract, not a paid API. OCR is always wired in production; if it is somehow
// absent (d.ocr == nil) the original error is surfaced instead.
func (d *DispatchExtractor) Extract(ctx context.Context, pdf []byte) ([]PageText, bool, string, error) {
	if d.html != nil && looksLikeHTML(pdf) {
		return d.html.Extract(ctx, pdf)
	}
	pages, hasTextLayer, version, err := d.textLayer.Extract(ctx, pdf)
	if err != nil {
		if d.ocr == nil {
			return nil, false, version, err
		}
		return d.ocr.Extract(ctx, pdf)
	}
	if hasTextLayer {
		return pages, true, version, nil
	}
	return d.ocr.Extract(ctx, pdf)
}

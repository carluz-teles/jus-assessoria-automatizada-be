package extraction

import "context"

// DispatchExtractor is the composite TextExtractor: it tries the text-layer adapter first
// (Fatia 5, cheap + pure-Go) and, when that reports no usable text layer (a scanned PDF),
// falls back to the OCR adapter (Fatia 6, Claude vision). This is the extractor the use case
// actually holds; the two leaf adapters stay single-responsibility behind the same port.
type DispatchExtractor struct {
	textLayer TextExtractor
	ocr       TextExtractor
}

// NewDispatchExtractor wires the dispatcher to the two leaf adapters. Both are injected as
// the TextExtractor port so the worker composes concrete adapters and tests substitute
// fakes.
func NewDispatchExtractor(textLayer, ocr TextExtractor) *DispatchExtractor {
	return &DispatchExtractor{textLayer: textLayer, ocr: ocr}
}

var _ TextExtractor = (*DispatchExtractor)(nil)

// Extract runs the text-layer adapter; if it succeeds AND found a real text layer, that
// result wins (its version = "pdftext-v1"). Otherwise — no text layer (a scan) — it routes
// to OCR and returns the OCR result (version = "claude-ocr-opus-4-8", hasTextLayer=false). A
// text-layer error is NOT swallowed into an OCR attempt: a corrupt PDF the reader rejects is
// terminal (invalid), and OCRing the same corrupt bytes would only spend an API call to fail
// again — the error surfaces so the listener archives it.
func (d *DispatchExtractor) Extract(ctx context.Context, pdf []byte) ([]PageText, bool, string, error) {
	pages, hasTextLayer, version, err := d.textLayer.Extract(ctx, pdf)
	if err != nil {
		return nil, false, version, err
	}
	if hasTextLayer {
		return pages, true, version, nil
	}
	return d.ocr.Extract(ctx, pdf)
}

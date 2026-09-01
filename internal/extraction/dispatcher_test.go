package extraction

import (
	"context"
	"errors"
	"testing"

	"github.com/jusassessoria/platform/lib/apperr"
)

// TestDispatchExtractor_TextLayerWins: when the text-layer adapter reports a real text layer,
// its result is returned and OCR is NEVER called.
func TestDispatchExtractor_TextLayerWins(t *testing.T) {
	tl := &fakeExtractor{pages: []PageText{{Page: 1, Text: "text"}}, hasTextLayer: true, version: "pdftext-v1"}
	ocr := &fakeExtractor{pages: []PageText{{Page: 1, Text: "ocr"}}, version: "claude-ocr-opus-4-8"}
	d := NewDispatchExtractor(tl, ocr)

	pages, has, version, err := d.Extract(context.Background(), []byte("pdf"))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !has || version != "pdftext-v1" || pages[0].Text != "text" {
		t.Errorf("got %v/%q/%+v, want text-layer result", has, version, pages)
	}
	if ocr.calls != 0 {
		t.Errorf("OCR called despite a text layer (calls = %d)", ocr.calls)
	}
}

// TestDispatchExtractor_OCRFallback: no text layer routes to OCR, and the OCR result (version,
// has_text_layer=false) is returned.
func TestDispatchExtractor_OCRFallback(t *testing.T) {
	tl := &fakeExtractor{pages: []PageText{{Page: 1, Text: ""}}, hasTextLayer: false, version: "pdftext-v1"}
	ocr := &fakeExtractor{pages: []PageText{{Page: 1, Text: "ocr"}}, hasTextLayer: false, version: "claude-ocr-opus-4-8"}
	d := NewDispatchExtractor(tl, ocr)

	pages, has, version, err := d.Extract(context.Background(), []byte("scan"))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if has || version != "claude-ocr-opus-4-8" || pages[0].Text != "ocr" {
		t.Errorf("got %v/%q/%+v, want OCR result", has, version, pages)
	}
	if ocr.calls != 1 {
		t.Errorf("OCR calls = %d, want 1", ocr.calls)
	}
}

// TestDispatchExtractor_ReaderErrorFallsBackToOCR: the pure-Go reader can't open some valid
// PDFs (2.0/advanced) that poppler can — so a text-layer open error routes to OCR, whose result
// is returned.
func TestDispatchExtractor_ReaderErrorFallsBackToOCR(t *testing.T) {
	tl := &fakeExtractor{err: apperr.NewInvalid("open pdf"), version: "pdftext-v2"}
	ocr := &fakeExtractor{pages: []PageText{{Page: 1, Text: "ocr"}}, version: "claude-ocr-opus-4-8"}
	d := NewDispatchExtractor(tl, ocr)

	pages, has, version, err := d.Extract(context.Background(), []byte("pdf2.0"))
	if err != nil {
		t.Fatalf("err = %v, want OCR fallback", err)
	}
	if has || version != "claude-ocr-opus-4-8" || pages[0].Text != "ocr" {
		t.Errorf("got %v/%q/%+v, want OCR result", has, version, pages)
	}
	if ocr.calls != 1 {
		t.Errorf("OCR calls = %d, want 1", ocr.calls)
	}
}

// TestDispatchExtractor_ReaderErrorSurfacesWhenOCRAlsoFails: if OCR also can't render the bytes
// (genuinely corrupt), its error surfaces so the listener retries/archives.
func TestDispatchExtractor_ReaderErrorSurfacesWhenOCRAlsoFails(t *testing.T) {
	boom := apperr.NewInvalid("ocr rasterize failed")
	tl := &fakeExtractor{err: apperr.NewInvalid("open pdf"), version: "pdftext-v2"}
	ocr := &fakeExtractor{err: boom}
	d := NewDispatchExtractor(tl, ocr)

	_, _, _, err := d.Extract(context.Background(), []byte("corrupt"))
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the OCR error", err)
	}
}

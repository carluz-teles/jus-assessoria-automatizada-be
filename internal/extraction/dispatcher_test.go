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

// TestDispatchExtractor_TextLayerErrorIsTerminal: a text-layer error (a corrupt PDF) is
// surfaced, NOT swallowed into an OCR attempt on the same bad bytes.
func TestDispatchExtractor_TextLayerErrorIsTerminal(t *testing.T) {
	boom := apperr.NewInvalid("open pdf")
	tl := &fakeExtractor{err: boom, version: "pdftext-v1"}
	ocr := &fakeExtractor{}
	d := NewDispatchExtractor(tl, ocr)

	_, _, _, err := d.Extract(context.Background(), []byte("corrupt"))
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the text-layer error", err)
	}
	if ocr.calls != 0 {
		t.Errorf("OCR called after a text-layer error (calls = %d)", ocr.calls)
	}
}

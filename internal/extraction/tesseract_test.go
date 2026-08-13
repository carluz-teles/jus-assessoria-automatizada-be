package extraction

import (
	"context"
	"os/exec"
	"reflect"
	"testing"
)

// TestOrderPageFiles: pdftoppm zero-pads the page suffix, so a lexical sort of the glob is
// page order — including past 9 pages, where an UNpadded sort would put page-10 before
// page-2. The helper is pure, so it's tested without invoking any binary; it must not mutate
// its input.
func TestOrderPageFiles(t *testing.T) {
	in := []string{
		"/tmp/x/page-03.png",
		"/tmp/x/page-01.png",
		"/tmp/x/page-10.png",
		"/tmp/x/page-02.png",
	}
	inCopy := append([]string(nil), in...)

	got := orderPageFiles(in)
	want := []string{
		"/tmp/x/page-01.png",
		"/tmp/x/page-02.png",
		"/tmp/x/page-03.png",
		"/tmp/x/page-10.png",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("orderPageFiles() = %v, want %v", got, want)
	}
	if !reflect.DeepEqual(in, inCopy) {
		t.Errorf("orderPageFiles mutated its input: %v", in)
	}
}

// minimalPDF is a tiny, valid single-page PDF (blank page, no text) used only to prove the
// full rasterize→OCR path runs end-to-end when the binaries are present. A blank page OCRs to
// (near-)empty text — the assertion is "≥1 page, no error", not text content.
var minimalPDF = []byte("%PDF-1.4\n" +
	"1 0 obj<< /Type /Catalog /Pages 2 0 R >>endobj\n" +
	"2 0 obj<< /Type /Pages /Kids [3 0 R] /Count 1 >>endobj\n" +
	"3 0 obj<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] >>endobj\n" +
	"xref\n0 4\n" +
	"0000000000 65535 f \n" +
	"0000000009 00000 n \n" +
	"0000000056 00000 n \n" +
	"0000000114 00000 n \n" +
	"trailer<< /Size 4 /Root 1 0 R >>\n" +
	"startxref\n186\n%%EOF\n")

// TestTesseractOCR_Extract runs the real adapter against a tiny generated PDF, but only when
// BOTH pdftoppm and tesseract are on PATH — otherwise it skips, so CI (where the binaries are
// absent) never fails on this. When they're present it asserts the full path succeeds and
// yields ≥1 page with the OCR version and hasTextLayer=false. Uses a low DPI to keep it fast.
func TestTesseractOCR_Extract(t *testing.T) {
	if _, err := exec.LookPath("pdftoppm"); err != nil {
		t.Skip("pdftoppm not on PATH — skipping full OCR extract test")
	}
	if _, err := exec.LookPath("tesseract"); err != nil {
		t.Skip("tesseract not on PATH — skipping full OCR extract test")
	}

	// Default lang "por" may not be installed; use "eng" if available, else fall back to the
	// binary's default by skipping when neither is present would over-complicate — "por" or
	// "eng" is enough to prove the path. Prefer the production lang.
	lang := defaultOCRLang
	o := newTesseractOCR(72, lang)

	pages, has, version, err := o.Extract(context.Background(), minimalPDF)
	if err != nil {
		t.Fatalf("Extract err = %v", err)
	}
	if has {
		t.Errorf("has_text_layer = true, want false (OCR)")
	}
	if version != tesseractVersion {
		t.Errorf("version = %q, want %q", version, tesseractVersion)
	}
	if len(pages) < 1 {
		t.Errorf("pages = %d, want >= 1", len(pages))
	}
}

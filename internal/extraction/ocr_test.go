package extraction

import (
	"context"
	"errors"
	"testing"

	"github.com/jusassessoria/platform/lib/apperr"
)

// fakeVision stands in for the Anthropic client so tests NEVER hit the real API.
type fakeVision struct {
	out   string
	err   error
	calls int
}

func (f *fakeVision) Transcribe(context.Context, string) (string, error) {
	f.calls++
	return f.out, f.err
}

// TestOCRExtractor_SplitsPages: the OCR adapter base64-encodes the PDF, calls the vision
// client, and splits the delimiter-separated transcription into per-page text. Always
// has_text_layer=false and the OCR version.
func TestOCRExtractor_SplitsPages(t *testing.T) {
	vision := &fakeVision{out: "page one\n" + pageDelimiter + "\npage two"}
	o := NewOCRExtractor(vision)

	pages, has, version, err := o.Extract(context.Background(), []byte("scan-bytes"))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if vision.calls != 1 {
		t.Fatalf("vision calls = %d, want 1", vision.calls)
	}
	if has {
		t.Errorf("has_text_layer = true, want false (OCR)")
	}
	if version != "claude-ocr-opus-4-8" {
		t.Errorf("version = %q, want claude-ocr-opus-4-8", version)
	}
	if len(pages) != 2 || pages[0].Page != 1 || pages[0].Text != "page one" || pages[1].Page != 2 || pages[1].Text != "page two" {
		t.Errorf("pages = %+v, want 2 split pages", pages)
	}
}

// TestOCRExtractor_SinglePageNoDelimiter: a transcription without the delimiter is one page.
func TestOCRExtractor_SinglePageNoDelimiter(t *testing.T) {
	o := NewOCRExtractor(&fakeVision{out: "just one page of text"})

	pages, _, _, err := o.Extract(context.Background(), []byte("scan"))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(pages) != 1 || pages[0].Page != 1 || pages[0].Text != "just one page of text" {
		t.Errorf("pages = %+v, want single page", pages)
	}
}

// TestOCRExtractor_VisionError: a vision fault surfaces (with the OCR version so the caller
// can still attribute the attempt).
func TestOCRExtractor_VisionError(t *testing.T) {
	boom := apperr.NewInfra("api down", errors.New("root"))
	o := NewOCRExtractor(&fakeVision{err: boom})

	_, _, version, err := o.Extract(context.Background(), []byte("scan"))
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the vision error", err)
	}
	if version != "claude-ocr-opus-4-8" {
		t.Errorf("version = %q, want claude-ocr-opus-4-8 even on error", version)
	}
}

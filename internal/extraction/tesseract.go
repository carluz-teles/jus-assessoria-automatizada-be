package extraction

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/jusassessoria/platform/lib/apperr"
)

// tesseractVersion identifies the deterministic OCR adapter in extractor_version. It is a
// pinned string (adapter + language), NOT the binary's version, so re-OCR is auditable and a
// pipeline change (a new lang model, a new rasterize pipeline) is a deliberate bump.
const tesseractVersion = "tesseract-por-v1"

const (
	// defaultOCRDPI is the rasterization resolution. 300 DPI is the Tesseract sweet spot for
	// scanned court documents: high enough for clean glyphs, low enough to keep memory/CPU
	// sane on a 70+ page scan.
	defaultOCRDPI = 300
	// defaultOCRLang is the Tesseract language pack. "por" (Portuguese) matches Brazilian
	// court PDFs; the image must carry tesseract-ocr-por.
	defaultOCRLang = "por"
)

// TesseractOCR is the deterministic OCR adapter: for scanned PDFs (no usable text layer) it
// rasterizes each page to PNG with pdftoppm and runs the tesseract CLI per page, capturing the
// recognized text. It has NO per-page LLM cost and needs no API key — the trade-off is that
// the worker-documents image must ship the poppler-utils + tesseract-ocr binaries (the
// distroless static image can't run them; see the runtime-ocr Docker stage). It shells out
// (no cgo) so the build stays CGO_ENABLED=0. It sits behind the same TextExtractor port;
// hasTextLayer is ALWAYS false — by definition OCR only runs on a document that had no usable
// text layer.
type TesseractOCR struct {
	dpi  int
	lang string
}

// NewTesseractOCR returns the OCR adapter with production defaults (300 DPI, Portuguese).
// Stateless apart from its config; the worker injects the zero-config constructor.
func NewTesseractOCR() *TesseractOCR {
	return newTesseractOCR(defaultOCRDPI, defaultOCRLang)
}

// newTesseractOCR is the unexported constructor for tests to override the DPI/lang (e.g. a
// lower DPI for a fast fixture round-trip).
func newTesseractOCR(dpi int, lang string) *TesseractOCR {
	return &TesseractOCR{dpi: dpi, lang: lang}
}

var _ TextExtractor = (*TesseractOCR)(nil)

// Extract writes the PDF to a private temp dir, rasterizes every page to PNG with pdftoppm,
// then runs tesseract per page and collects the recognized text into per-page PageText
// (1-based). A missing binary, a non-zero exit, or a rasterize failure is a retryable infra
// error (the same bytes may succeed on a healthy node); a 0-page PDF (nothing rasterized) is a
// terminal invalid. The temp dir (0700) is always removed. hasTextLayer is always false; the
// version is the pinned OCR version.
func (t *TesseractOCR) Extract(ctx context.Context, data []byte) ([]PageText, bool, string, error) {
	dir, err := os.MkdirTemp("", "ocr-*")
	if err != nil {
		return nil, false, tesseractVersion, apperr.NewInfra("extraction: tesseract mkdtemp", err)
	}
	defer os.RemoveAll(dir)

	inPDF := filepath.Join(dir, "in.pdf")
	if err := os.WriteFile(inPDF, data, 0o600); err != nil {
		return nil, false, tesseractVersion, apperr.NewInfra("extraction: tesseract write pdf", err)
	}

	// pdftoppm zero-pads the page-number suffix to the page-count width (page-01.png …
	// page-71.png), so a lexical sort of the glob equals page order.
	prefix := filepath.Join(dir, "page")
	if err := t.runRasterize(ctx, inPDF, prefix); err != nil {
		return nil, false, tesseractVersion, err
	}

	imgs, err := filepath.Glob(prefix + "-*.png")
	if err != nil {
		return nil, false, tesseractVersion, apperr.NewInfra("extraction: tesseract glob pages", err)
	}
	imgs = orderPageFiles(imgs)
	if len(imgs) == 0 {
		return nil, false, tesseractVersion, apperr.NewInvalid("extraction: tesseract produced no pages (0-page pdf?)")
	}

	pages := make([]PageText, 0, len(imgs))
	for i, img := range imgs {
		text, err := t.runTesseract(ctx, img)
		if err != nil {
			return nil, false, tesseractVersion, err
		}
		pages = append(pages, PageText{Page: i + 1, Text: strings.TrimSpace(text)})
	}
	return pages, false, tesseractVersion, nil
}

// runRasterize shells out to pdftoppm to render every page of inPDF to <prefix>-NN.png at the
// configured DPI. A missing binary or a non-zero exit is wrapped as retryable infra with the
// captured stderr for debuggability.
func (t *TesseractOCR) runRasterize(ctx context.Context, inPDF, prefix string) error {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "pdftoppm", "-png", "-r", strconv.Itoa(t.dpi), inPDF, prefix)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return apperr.NewInfra("extraction: tesseract rasterize (pdftoppm): "+strings.TrimSpace(stderr.String()), err)
	}
	return nil
}

// runTesseract shells out to the tesseract CLI to OCR one page image, capturing recognized
// text from stdout. A missing binary or a non-zero exit is wrapped as retryable infra with the
// captured stderr for debuggability.
func (t *TesseractOCR) runTesseract(ctx context.Context, img string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "tesseract", img, "stdout", "-l", t.lang)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", apperr.NewInfra("extraction: tesseract ocr: "+strings.TrimSpace(stderr.String()), err)
	}
	return stdout.String(), nil
}

// orderPageFiles sorts the rasterized page files into page order. pdftoppm zero-pads the
// numeric suffix to a fixed width, so a plain lexical sort is already page order; sorting is
// factored out as a pure helper (glob → sort → the i+1 page mapping the caller applies) so the
// ordering is unit-testable without invoking any binary. Returns a new sorted slice (does not
// mutate the input) so the caller's glob result stays intact.
func orderPageFiles(files []string) []string {
	out := make([]string, len(files))
	copy(out, files)
	sort.Strings(out)
	return out
}

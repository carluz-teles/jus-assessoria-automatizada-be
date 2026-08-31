package extraction

import (
	"log/slog"
	"net/http"

	"github.com/hibiken/asynq"

	"github.com/jusassessoria/platform/lib/database"
)

// register.go is the slice's composition seam — the ONE function the worker calls to mount
// the extraction pipeline. It assembles the adapters (storage, repo, dedup, outbox, the
// text-layer+OCR dispatcher) into the use case, wraps it in the listener, and registers the
// handler on the mux. Mirrors how internal/deadline exposes its listener registration: the
// worker composes, the slice self-contains its wiring.

// StoragePresigner is the lib/storage capability this slice needs — DIRECT object byte access
// (GetBytes/PutBytes). The worker's *storage.Client satisfies it. Name kept for call-site
// stability; it is an alias of the internal objectStore port (server-side reads go direct, not
// via presigned URLs — see storage.go).
type StoragePresigner = backendStore

// Deps carries everything RegisterExtractionListeners needs from the worker's composition
// root. All are ports/values the worker already holds: the unit of work (tenant-scoped write
// txs), the storage presigner, the transactional outbox, and the OCR selection/credential.
// OCREngine picks the OCR adapter: "tesseract" (the default) needs NO key — it shells out to
// the deterministic, free OCR binaries baked into the runtime-ocr image; AnthropicAPIKey is
// used ONLY when OCREngine=="claude" (opt-in Claude-vision OCR). Vision overrides both — inject
// a fake in tests. HTTPClient is optional (nil → a defaulted client).
type Deps struct {
	UoW             database.UnitOfWork
	Storage         StoragePresigner
	Outbox          publisher
	Vision          visionClient // optional: inject to override the OCR client (tests/fakes)
	OCREngine       string       // "tesseract" (default, free) | "claude"; unset → tesseract
	AnthropicAPIKey string       // used to build the real vision client only when OCREngine=="claude"
	HTTPClient      *http.Client // optional: nil → defaulted
	// Logger is the worker's structured logger, threaded into the use case for the
	// successful-extraction telemetry line ("document extracted"). Required (the worker
	// always holds one); nil would panic on the first log.
	Logger *slog.Logger
}

// RegisterExtractionListeners composes the extraction slice and mounts its handler on mux.
// The worker calls this once with a fully-populated Deps; adding this slice's async surface
// is one line in the worker's composition. It selects the OCR adapter (deterministic Tesseract
// by default, Claude vision opt-in, or an injected fake), wires the text-layer+OCR dispatcher,
// and hands the use case to the listener.
func RegisterExtractionListeners(mux *asynq.ServeMux, deps Deps) error {
	var ocr TextExtractor
	switch {
	case deps.Vision != nil: // test/override: use the injected vision client
		ocr = NewOCRExtractor(deps.Vision)
	case deps.OCREngine == "claude": // opt-in: paid Claude-vision OCR
		ocr = NewOCRExtractor(NewAnthropicVision(deps.AnthropicAPIKey))
	default: // "tesseract" (default) or unset — deterministic, free, no key
		ocr = NewTesseractOCR()
	}

	extractor := NewDispatchExtractor(NewTextLayerExtractor(), ocr).WithHTMLExtractor(NewHTMLExtractor())
	store := NewStorage(deps.Storage, deps.HTTPClient)

	uc, err := NewUseCase(deps.UoW, store, NewRepository(), extractor, NewDedup(), deps.Outbox, deps.Logger)
	if err != nil {
		return err
	}
	NewListener(uc).Register(mux)
	return nil
}

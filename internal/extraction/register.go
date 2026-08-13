package extraction

import (
	"net/http"

	"github.com/hibiken/asynq"

	"github.com/jusassessoria/platform/lib/database"
)

// register.go is the slice's composition seam — the ONE function the worker calls to mount
// the extraction pipeline. It assembles the adapters (storage, repo, dedup, outbox, the
// text-layer+OCR dispatcher) into the use case, wraps it in the listener, and registers the
// handler on the mux. Mirrors how internal/deadline exposes its listener registration: the
// worker composes, the slice self-contains its wiring.

// StoragePresigner is the lib/storage capability this slice needs — presigned GET/PUT. The
// worker's *storage.Client satisfies it (its PresignedGet/PresignedPut match). It is an alias
// of the internal presigner port so the Deps surface names the capability the worker binds.
type StoragePresigner = presigner

// Deps carries everything RegisterExtractionListeners needs from the worker's composition
// root. All are ports/values the worker already holds: the unit of work (tenant-scoped write
// txs), the storage presigner, the transactional outbox, and the OCR credential. Exactly ONE
// of Vision / AnthropicAPIKey is required — inject a Vision fake in tests, or the API key in
// production (the slice builds the real Anthropic client from it, so the worker needs no SDK
// import). HTTPClient is optional (nil → a defaulted client).
type Deps struct {
	UoW             database.UnitOfWork
	Storage         StoragePresigner
	Outbox          publisher
	Vision          visionClient // optional: inject to override the OCR client (tests/fakes)
	AnthropicAPIKey string       // used to build the real vision client when Vision is nil
	HTTPClient      *http.Client // optional: nil → defaulted
}

// RegisterExtractionListeners composes the extraction slice and mounts its handler on mux.
// The worker calls this once with a fully-populated Deps; adding this slice's async surface
// is one line in the worker's composition. It builds the OCR client from Deps.Vision (if
// set) else Deps.AnthropicAPIKey, wires the text-layer+OCR dispatcher, and hands the use case
// to the listener.
func RegisterExtractionListeners(mux *asynq.ServeMux, deps Deps) {
	vision := deps.Vision
	if vision == nil {
		vision = NewAnthropicVision(deps.AnthropicAPIKey)
	}

	extractor := NewDispatchExtractor(NewTextLayerExtractor(), NewOCRExtractor(vision))
	store := NewStorage(deps.Storage, deps.HTTPClient)

	uc := NewUseCase(deps.UoW, store, NewRepository(), extractor, NewDedup(), deps.Outbox)
	NewListener(uc).Register(mux)
}

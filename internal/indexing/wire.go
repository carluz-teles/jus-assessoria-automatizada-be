package indexing

import (
	"log/slog"

	"github.com/hibiken/asynq"

	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// wire.go is the fatia's composition surface — the ONE thing the worker binary calls. The slice
// exposes RegisterIndexingListeners(mux, deps); the worker builds Deps and calls it, so main.go
// never reaches into the slice's internals. Mirrors how the worker composes the deadline slice.

// Deps carries the concrete collaborators the indexing pipeline needs, injected by the worker.
// Everything except the config scalars is an interface the slice already declares, so the worker
// passes the real adapters (this package's NewVoyageEmbedder / NewStorageReader / NewRepository /
// NewDedup, plus the shared database.UnitOfWork and *events.Outbox) and tests are unaffected.
type Deps struct {
	// UOW is the tenant-scoped write transaction boundary (database.NewUnitOfWork(pool)).
	UOW database.UnitOfWork
	// Storage is the object storage client used to read the extracted-text JSON. It only needs a
	// direct byte read (the objectByteReader port); pass the *storage.Client.
	Storage objectByteReader
	// Outbox is the transactional-outbox producer (events.NewOutbox()).
	Outbox *events.Outbox
	// Embedder is the embeddings port (NewVoyageEmbedder(cfg.VoyageAPIKey, cfg.VoyageModel,
	// cfg.VoyageEmbedDim, httpClient)).
	Embedder Embedder
	// EmbedDim is cfg.VoyageEmbedDim — the dimensionality stamped on each chunk row (provenance).
	EmbedDim int
	// Logger is the worker's structured logger, threaded into the use case for the
	// successful-indexing telemetry line ("document indexed"). Required (the worker always holds
	// one); nil would panic on the first log.
	Logger *slog.Logger
}

// RegisterIndexingListeners builds the indexing use case from deps and mounts its consumer on the
// asynq mux. The worker calls it once, after building Deps.
//
// WIRING NOTE (routing): the consumed event is document.extracted, whose relay queue is decided by
// lib/events.queueFor on the "document" prefix. document.* currently falls through to the
// "default" queue (queueFor matches "documents", not "document"). For this listener to receive the
// event, worker-documents' asynq server must serve whatever queue the relay routes document.* to.
// That routing is the composer's concern (it lives in lib/events, outside this fatia's ownership);
// this slice only registers the task-type handler on the mux it is given.
func RegisterIndexingListeners(mux *asynq.ServeMux, deps Deps) error {
	uc, err := NewUseCase(
		NewStorageReader(deps.Storage, nil),
		deps.Embedder,
		deps.UOW,
		NewDedup(),
		NewRepository(),
		deps.Outbox,
		deps.EmbedDim,
		deps.Logger,
	)
	if err != nil {
		return err
	}
	NewListener(uc).Register(mux)
	return nil
}

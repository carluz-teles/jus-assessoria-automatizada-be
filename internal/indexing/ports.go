package indexing

import (
	"context"

	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// ports.go declares the narrow ports the indexing use case depends on — every side effect
// (embed, storage read, dedup, outbox, DB writes) behind an interface so the pipeline is
// unit-testable with fakes and never hits the real Voyage API, storage, or a DB in tests. The
// binary composes the concrete adapters (Deps in wire.go); tests substitute fakes.

// consumerIndexing is the processed_event consumer name this slice dedups under. Dedup is
// per-consumer (docs §4c.3), so it is slice-specific — marking here never blocks another
// consumer of the same document.extracted event.
const consumerIndexing = "indexing"

// InputType tells the (asymmetric) embedding model which side of the retrieval it is embedding.
// Voyage embeds queries and documents into slightly different sub-spaces: the corpus/indexing side
// must use InputDocument, the retrieval-query side InputQuery. Sending a query as a document (the
// old bug) measurably hurts recall. It is a named string (not a bool) so call sites read as
// self-documenting and a future model with more modes extends without a signature churn.
type InputType string

const (
	// InputDocument is the indexing side — corpus chunks being embedded for storage.
	InputDocument InputType = "document"
	// InputQuery is the retrieval side — the RAG query being embedded to search the corpus.
	InputQuery InputType = "query"
)

// Embedder turns texts into embedding vectors. inputType selects the query vs document sub-space
// (Voyage is asymmetric — see InputType). It returns the vectors (one per input, in order), the
// model id that produced them (stored as chunk.embedding_model), and an error. Kept behind this
// port so the pipeline never depends on Voyage directly — tests inject a fake and the real API is
// never called under test.
type Embedder interface {
	Embed(ctx context.Context, texts []string, inputType InputType) ([][]float32, string, error)
}

// objectReader reads an object's raw bytes from storage by key. The storage lib only presigns
// URLs (no direct read), so the concrete adapter presigns a GET then http.Gets it; the port hides
// that so the pipeline just asks for the extracted-text JSON bytes and tests return canned bytes.
type objectReader interface {
	ReadObject(ctx context.Context, key string) ([]byte, error)
}

// deduper is the consumer-side idempotency guard port. It marks (consumer, eventID) within the
// caller's tx and reports whether it was already there (a replay), so the mark and the chunk
// writes commit atomically. Mirrors internal/deadline's deduper.
type deduper interface {
	SeenOrMark(ctx context.Context, tx database.Tx, consumer, eventID string) (seen bool, err error)
}

// outbox is the transactional-outbox producer port: Publish writes the event's row through the
// caller's tx, so the chunk writes and the document.ready/failed row commit together. Satisfied
// by *events.Outbox.
type outbox interface {
	Publish(ctx context.Context, tx database.Tx, ev events.Event) error
}

// unitOfWork is the transaction boundary port: Do opens a tenant-scoped (RLS) write tx, runs fn,
// and commits (or rolls back on error). Satisfied by *database's UnitOfWork; tests inject a fake
// that runs fn with a nil tx.
type unitOfWork interface {
	Do(ctx context.Context, tenantID string, fn func(tx database.Tx) error) error
}

// repository is the DB-write port the pipeline drives — the raw-pgx chunk INSERT and the two
// document status transitions (EXTRACTED→CHUNKED→READY, and →FAILED with the error). Every method
// binds to the caller's tx; the repo is stateless. Tests inject a fake to assert the writes.
type repository interface {
	// InsertChunks writes the chunks for a document with ON CONFLICT (document_id, page,
	// chunk_hash) DO NOTHING (idempotent reprocess), stamping embedding/embedding_model/dim. It
	// returns the number of rows actually inserted (conflicts skipped).
	InsertChunks(ctx context.Context, tx database.Tx, rows []ChunkRow) (int, error)
	// SetStatus updates a document's status column (EXTRACTED→CHUNKED→READY) scoped to tenantID.
	SetStatus(ctx context.Context, tx database.Tx, documentID, tenantID, status string) error
	// SetFailed sets a document's status to FAILED and writes the {stage,message} error jsonb,
	// scoped to tenantID.
	SetFailed(ctx context.Context, tx database.Tx, documentID, tenantID, stage, message string) error
}

// Document status values the pipeline transitions through (mirrors the document_status_check
// constraint from migration 0033). The indexing fatia only writes the last leg of the saga:
// EXTRACTED (on entry) → CHUNKED (chunks persisted) → READY (done); or → FAILED on any error.
const (
	statusChunked = "CHUNKED"
	statusReady   = "READY"
	statusFailed  = "FAILED"
)

// ChunkRow is one chunk ready to persist: the document + page it belongs to, its text, the
// embedding vector as []float32 (the repo wraps it in pgvector.Vector), the model id, the vector
// dimensionality and the content hash (the dedup key). It is the pipeline→repo write shape.
type ChunkRow struct {
	DocumentID     string
	Page           int
	Text           string
	Embedding      []float32
	EmbeddingModel string
	Dim            int
	Hash           string
}

package indexing

import (
	"context"

	"github.com/google/uuid"
	pgvector "github.com/pgvector/pgvector-go"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/database"
)

// repository.go is the raw-pgx write port of the fatia (there is no sqlc for chunk — it is not in
// the generated set, and the vector column is hand-bound via pgvector). Every method binds to the
// caller's tx (all writes are transactional, so RLS scopes them on top of the explicit tenant
// filter on document); the repo is stateless.

// pgRepository is the pgx-backed repository. It holds no pool — each method binds to the tx it is
// given, so the use case owns the transaction boundary.
type pgRepository struct{}

var _ repository = (*pgRepository)(nil)

// NewRepository returns the repository. Stateless: nothing to inject at construction.
func NewRepository() repository { return &pgRepository{} }

// insertChunk inserts one chunk row, idempotent on the (document_id, page, chunk_hash) unique
// index (migration 0034). ON CONFLICT DO NOTHING makes a reprocess a no-op for already-indexed
// chunks. The embedding binds through pgvector.Vector (driver.Valuer → the text "[..]" the
// vector column parses); no pool type registration is needed for the write path.
const insertChunk = `INSERT INTO chunk
	(document_id, page, text, embedding, embedding_model, dim, chunk_hash)
	VALUES ($1, $2, $3, $4, $5, $6, $7)
	ON CONFLICT (document_id, page, chunk_hash) DO NOTHING`

// InsertChunks writes each chunk row within tx, counting the rows actually inserted (a conflict
// skips silently, contributing 0). Bytes go straight to the vector column via pgvector.NewVector.
// A malformed document_id uuid is a terminal invalid; any exec failure is infra (retryable).
func (r *pgRepository) InsertChunks(ctx context.Context, tx database.Tx, rows []ChunkRow) (int, error) {
	inserted := 0
	for _, row := range rows {
		docID, err := parseUUID(row.DocumentID)
		if err != nil {
			return inserted, err
		}
		tag, err := tx.Exec(ctx, insertChunk,
			docID,
			row.Page,
			row.Text,
			pgvector.NewVector(row.Embedding),
			row.EmbeddingModel,
			row.Dim,
			row.Hash,
		)
		if err != nil {
			return inserted, database.WrapInfra(err)
		}
		inserted += int(tag.RowsAffected())
	}
	return inserted, nil
}

// setStatus is the guarded document status UPDATE — scoped to (id, tenant_id) so a foreign
// document is never touched (barrier 1 on top of RLS). It filters deleted_at IS NULL: a
// soft-deleted document is not re-indexed.
const setStatus = `UPDATE document
	SET status = $3
	WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL`

// SetStatus flips a document's status column (EXTRACTED→CHUNKED→READY). A no-match (a gone /
// foreign / soft-deleted document) is the typed ErrDocumentNotFound (terminal → the listener
// archives). Any exec failure is infra (retryable).
func (r *pgRepository) SetStatus(ctx context.Context, tx database.Tx, documentID, tenantID, status string) error {
	docID, err := parseUUID(documentID)
	if err != nil {
		return err
	}
	tenID, err := parseUUID(tenantID)
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, setStatus, docID, tenID, status)
	if err != nil {
		return database.WrapInfra(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrDocumentNotFound
	}
	return nil
}

// setFailed writes the FAILED status + the {stage,message} error jsonb (migration 0033's error
// column), scoped to (id, tenant_id). Not gated on deleted_at: recording a failure on a document
// that raced a delete is harmless.
const setFailed = `UPDATE document
	SET status = 'FAILED',
	    error = jsonb_build_object('stage', $3::text, 'message', $4::text)
	WHERE id = $1 AND tenant_id = $2`

// SetFailed flips a document to FAILED with the failure's {stage,message}. A no-match is NOT an
// error here (the failure path is best-effort — a gone document needs no failure record); the
// caller ignores the result and always surfaces the original cause. Any exec failure is infra.
func (r *pgRepository) SetFailed(ctx context.Context, tx database.Tx, documentID, tenantID, stage, message string) error {
	docID, err := parseUUID(documentID)
	if err != nil {
		return err
	}
	tenID, err := parseUUID(tenantID)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, setFailed, docID, tenID, stage, message); err != nil {
		return database.WrapInfra(err)
	}
	return nil
}

// parseUUID validates an id string is a well-formed uuid before it reaches the query — a
// malformed id can never match on retry, so it is a terminal invalid. Returns the pgx-friendly
// string form on success (pgx v5 parses a string into the uuid column).
func parseUUID(s string) (string, error) {
	if _, err := uuid.Parse(s); err != nil {
		return "", apperr.NewInvalid("indexing: malformed uuid")
	}
	return s, nil
}

// ErrDocumentNotFound is the typed miss for a document status write that matched no row (gone /
// foreign / soft-deleted). It is a KindNotFound, so isTerminal archives the task rather than
// retrying forever against a document that will never appear.
var ErrDocumentNotFound = apperr.NewNotFound("document not found")

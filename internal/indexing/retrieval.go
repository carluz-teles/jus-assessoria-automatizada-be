package indexing

import (
	"context"

	"github.com/jackc/pgx/v5"
	pgvector "github.com/pgvector/pgvector-go"

	"github.com/jusassessoria/platform/lib/database"
)

// retrieval.go is the READ path of the fatia — the cosine-similarity search over chunks the
// advisory consumes later. It is exported (SearchChunks) but has NO HTTP handler: it is called
// in-process by the advisory slice. Tenant isolation is load-bearing here — a chunk leaked
// across tenants is leaked autos — so the query JOINs document and filters document.tenant_id.

// querier is the sliver of a pgx pool SearchChunks needs: a single Query. Depending on it (not
// *pgxpool.Pool) keeps the read unit-testable with a fake that asserts the SQL + args.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// SearchDeps carries what SearchChunks needs: the read pool. It is separate from the pipeline's
// Deps because retrieval is a read on the pool, not a write in a tenant tx — the tenant filter is
// an explicit WHERE (the caller is the advisory, which passes the tenant it already verified).
type SearchDeps struct {
	Pool querier
}

// ChunkHit is one retrieval result: the document + page the chunk came from, its text, and the
// cosine SIMILARITY score (1 - distance, so higher = closer). Ordered by distance ascending
// (most similar first) and capped at topK.
type ChunkHit struct {
	DocumentID string
	Page       int
	Text       string
	Score      float64
}

// searchChunksSQL ranks chunks by cosine distance to the query vector (embedding <=> $1, the
// vector_cosine_ops the 0034 HNSW index accelerates). It JOINs document to enforce tenant
// isolation (document.tenant_id = $2 — a chunk carries no tenant_id, so this is the ONLY barrier)
// and optionally scopes to one process (document.court_record_id = $3 when $3 is non-null).
// Soft-deleted documents are excluded. Score is 1 - distance (cosine similarity). $4 is topK.
const searchChunksSQL = `SELECT c.document_id, c.page, c.text, 1 - (c.embedding <=> $1) AS score
	FROM chunk c
	JOIN document d ON d.id = c.document_id
	WHERE d.tenant_id = $2
	  AND d.deleted_at IS NULL
	  AND ($3::uuid IS NULL OR d.court_record_id = $3::uuid)
	ORDER BY c.embedding <=> $1
	LIMIT $4`

// SearchChunks returns the topK chunks most similar (cosine) to query within tenantID, optionally
// scoped to courtRecordID (nil = across the tenant's whole corpus). Tenant isolation is enforced
// in SQL (the JOIN + document.tenant_id filter), so a caller cannot read another tenant's autos
// even by passing a foreign courtRecordID. The query vector binds through pgvector.Vector; a nil
// courtRecordID binds SQL NULL (the $3 IS NULL branch keeps the whole corpus in scope).
func SearchChunks(ctx context.Context, deps SearchDeps, tenantID string, courtRecordID *string, query []float32, topK int) ([]ChunkHit, error) {
	// A nil/empty courtRecordID → SQL NULL (whole-tenant scope); a set one binds as the uuid the
	// $3::uuid cast parses. any(nil) marshals to NULL through pgx.
	var crid any
	if courtRecordID != nil && *courtRecordID != "" {
		crid = *courtRecordID
	}

	rows, err := deps.Pool.Query(ctx, searchChunksSQL,
		pgvector.NewVector(query),
		tenantID,
		crid,
		topK,
	)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	defer rows.Close()

	var hits []ChunkHit
	for rows.Next() {
		var h ChunkHit
		if err := rows.Scan(&h.DocumentID, &h.Page, &h.Text, &h.Score); err != nil {
			return nil, database.WrapInfra(err)
		}
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		return nil, database.WrapInfra(err)
	}
	return hits, nil
}

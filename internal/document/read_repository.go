package document

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/jusassessoria/platform/internal/document/documentdb"
	"github.com/jusassessoria/platform/lib/database"
)

// read_repository.go is the pool-backed read adapter — the screen reads, off the transactional
// write path (which uses the stateless, tx-bound pgRepository). It holds its own documentdb
// bound to the pool; every query filters tenant_id explicitly (barrier 1) from the trusted
// principal, so a caller only ever sees its own documents. The mapper here absorbs the driver
// types so the read models stay pure.

// pgReadRepository serves the read port off the connection pool. Reads are not part of the use
// case's write tx, so the repo owns its own Queries (bound once at construction).
type pgReadRepository struct {
	q *documentdb.Queries
}

var _ readRepo = (*pgReadRepository)(nil)

// NewReadRepository returns the read port over the pool. Share nothing with the write repo: the
// read side never enrolls in the write transaction.
func NewReadRepository(pool documentdb.DBTX) readRepo {
	return &pgReadRepository{q: documentdb.New(pool)}
}

// ListDocumentsByProcesso reads one process's documents (descending keyset by created_at,
// newest first) on the pool, filtered by tenant_id and court_record_id (and deleted_at IS
// NULL). The caller passes the max sentinel cursor for the first page.
func (r *pgReadRepository) ListDocumentsByProcesso(ctx context.Context, q DocumentsByProcessoQuery) ([]DocumentView, error) {
	tid, err := parseUUID(q.TenantID)
	if err != nil {
		return nil, err
	}
	crid, err := parseUUID(q.CourtRecordID)
	if err != nil {
		return nil, err
	}
	lastID, err := parseUUID(q.LastID)
	if err != nil {
		return nil, err
	}

	rows, err := r.q.ListDocumentsByProcesso(ctx, documentdb.ListDocumentsByProcessoParams{
		CourtRecordID: crid,
		TenantID:      tid,
		LastCreated:   pgTimestamptz(q.LastCreated),
		LastID:        lastID,
		PageLimit:     int32(q.Limit),
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}

	out := make([]DocumentView, 0, len(rows))
	for _, row := range rows {
		out = append(out, viewFromListRow(row))
	}
	return out, nil
}

// CountDocumentsByProcesso returns the "X de Y" total for the Documentos tab, scoped by the same
// tenant + court_record (and deleted_at IS NULL) as the list.
func (r *pgReadRepository) CountDocumentsByProcesso(ctx context.Context, tenantID, courtRecordID string) (int64, error) {
	tid, err := parseUUID(tenantID)
	if err != nil {
		return 0, err
	}
	crid, err := parseUUID(courtRecordID)
	if err != nil {
		return 0, err
	}
	total, err := r.q.CountDocumentsByProcesso(ctx, documentdb.CountDocumentsByProcessoParams{
		CourtRecordID: crid,
		TenantID:      tid,
	})
	if err != nil {
		return 0, database.WrapInfra(err)
	}
	return total, nil
}

// GetDocument reads one document's detail on the pool, filtered by tenant_id (and deleted_at IS
// NULL). A miss — or a foreign/soft-deleted row — maps to the typed ErrDocumentNotFound (→ 404),
// never (nil, nil).
func (r *pgReadRepository) GetDocument(ctx context.Context, tenantID, id string) (DocumentView, error) {
	tid, err := parseUUID(tenantID)
	if err != nil {
		return DocumentView{}, err
	}
	did, err := parseUUID(id)
	if err != nil {
		return DocumentView{}, err
	}

	row, err := r.q.GetDocument(ctx, documentdb.GetDocumentParams{ID: did, TenantID: tid})
	if errors.Is(err, pgx.ErrNoRows) {
		return DocumentView{}, ErrDocumentNotFound
	}
	if err != nil {
		return DocumentView{}, database.WrapInfra(err)
	}
	return viewFromGetRow(row), nil
}

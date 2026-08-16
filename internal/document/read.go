package document

import (
	"context"
	"time"

	"github.com/jusassessoria/platform/lib/httpx"
)

// read.go is the slice's read side: the Documentos screen reads that bypass the write use case
// (a DTO per query, not the entity). It does NOT reuse the write Repository — the reads run on
// the pool with their own narrow port (readRepo). The ReadUseCase is a thin pagination policy
// over that port: it over-fetches one row to tell the handler whether a next page exists,
// without a separate COUNT for hasMore.

// DocumentView is the single wire shape of a document — the aba row (GET
// /v1/processos/:id/documentos), the detail (GET /v1/documentos/:id) AND the complete write
// result (POST /v1/documentos/:id/complete). So a document looks identical however it is read
// or written. The optional fields (court_record_id/original_filename/mime_type/size_bytes/
// pages/checksum) are omitted when absent; created_at serializes RFC3339 (Go's default for
// time.Time), and Status walks the pipeline saga.
type DocumentView struct {
	ID               string    `json:"id"`
	CourtRecordID    string    `json:"court_record_id,omitempty"`
	DocumentType     string    `json:"document_type"`
	Origin           string    `json:"origin"`
	Title            string    `json:"title"`
	OriginalFilename string    `json:"original_filename,omitempty"`
	MimeType         string    `json:"mime_type,omitempty"`
	SizeBytes        int64     `json:"size_bytes,omitempty"`
	Pages            int       `json:"pages,omitempty"`
	Status           string    `json:"status"`
	HasTextLayer     bool      `json:"has_text_layer"`
	Checksum         string    `json:"checksum,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
}

// DocumentsByProcessoQuery carries the descending keyset cursor (the last row's created_at and
// id) plus the process (court_record) whose documents to read and the tenant. The handler fills
// the max sentinel for a first page (newest-first, so the scan starts above every row).
type DocumentsByProcessoQuery struct {
	TenantID      string
	CourtRecordID string
	LastCreated   time.Time
	LastID        string
	Limit         int
}

// DocumentsByProcessoResult is a page of a process's documents plus its total for the "X de Y"
// counter. There is no filter on the aba, so a single Total carries both sides (mirrors the
// deadline Prazos tab); Filters is the envelope's chips block, always an empty object here.
type DocumentsByProcessoResult struct {
	Items   []DocumentView
	HasMore bool
	Total   int64
	Filters httpx.Filters
}

// readRepo is the narrow read port the ReadUseCase drives — the keyset list read, its counter,
// and the single-document detail. It is deliberately separate from the write Repository: reads
// run on the pool, off the transactional write path.
type readRepo interface {
	ListDocumentsByProcesso(ctx context.Context, q DocumentsByProcessoQuery) ([]DocumentView, error)
	CountDocumentsByProcesso(ctx context.Context, tenantID, courtRecordID string) (int64, error)
	// GetDocument reads one document's detail, scoped to tenantID (barrier 1). A miss (or a
	// foreign/soft-deleted id) is ErrDocumentNotFound (→ 404).
	GetDocument(ctx context.Context, tenantID, id string) (DocumentView, error)
}

// ReadUseCase serves the Documentos screen reads. It is a pagination policy over readRepo: it
// over-fetches one row per page so the handler learns whether more remain without a separate
// COUNT.
type ReadUseCase struct {
	repo readRepo
}

// NewReadUseCase wires the read use case over a read port (the pool-backed read repo).
func NewReadUseCase(repo readRepo) *ReadUseCase { return &ReadUseCase{repo: repo} }

// DocumentsByProcesso returns up to q.Limit of a process's documents (newest first), whether a
// further page exists, and the aba's total. The keyset read over-fetches one row for hasMore;
// the total is a separate COUNT — a small skew under concurrent inserts is fine (read model,
// not aggregate).
func (uc *ReadUseCase) DocumentsByProcesso(ctx context.Context, q DocumentsByProcessoQuery) (DocumentsByProcessoResult, error) {
	limit := q.Limit
	q.Limit = limit + 1
	rows, err := uc.repo.ListDocumentsByProcesso(ctx, q)
	if err != nil {
		return DocumentsByProcessoResult{}, err
	}
	hasMore := false
	if len(rows) > limit {
		rows, hasMore = rows[:limit], true
	}
	total, err := uc.repo.CountDocumentsByProcesso(ctx, q.TenantID, q.CourtRecordID)
	if err != nil {
		return DocumentsByProcessoResult{}, err
	}
	return DocumentsByProcessoResult{Items: rows, HasMore: hasMore, Total: total, Filters: httpx.Filters{}}, nil
}

// Document returns one document's detail, or the repo's typed ErrDocumentNotFound (→ 404) when
// the id resolves to no live row in the tenant.
func (uc *ReadUseCase) Document(ctx context.Context, tenantID, id string) (DocumentView, error) {
	return uc.repo.GetDocument(ctx, tenantID, id)
}

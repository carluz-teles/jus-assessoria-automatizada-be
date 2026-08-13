package document

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jusassessoria/platform/internal/document/documentdb"
	"github.com/jusassessoria/platform/lib/database"
)

// Repository is the write port the use case drives — the stateless, tx-bound sqlc writes of the
// upload → complete → delete path. Every method binds documentdb to the caller's tx (all writes
// are transactional, so RLS scopes them to the principal's tenant on top of the explicit tenant
// filter); the repo holds no pool of its own — the use case owns the boundary.
type Repository interface {
	// EnsureCourtRecordInTenant confirms the given court_record exists in the tenant (the guard
	// the upload start runs when a court_record_id is supplied). A miss → ErrCourtRecordNotFound
	// (→ 404), never a phantom document on a foreign/unknown process.
	EnsureCourtRecordInTenant(ctx context.Context, tx database.Tx, tenantID, courtRecordID string) error
	// InsertDocument persists a new UPLOAD document (BORN PENDING) with its storage_key set, and
	// returns it with the DB-assigned id + created_at so the handler renders the presign response.
	InsertDocument(ctx context.Context, tx database.Tx, d *Document) (*Document, error)
	// GetDocumentForComplete loads a document's upload state (id/tenant/status/storage_key/mime/
	// court_record) scoped to tenantID (barrier 1), filtering soft-deleted. A miss →
	// ErrDocumentNotFound (→ 404).
	GetDocumentForComplete(ctx context.Context, tx database.Tx, id, tenantID string) (*DocumentForComplete, error)
	// MarkUploaded flips the document PENDING→UPLOADED (guarded on status='PENDING') and sets the
	// optional checksum, scoped to tenantID. A no-match (racing complete / not PENDING) →
	// ErrDocumentNotFound. On a hit it returns the full document so document.uploaded commits with
	// it in the same tx.
	MarkUploaded(ctx context.Context, tx database.Tx, id, tenantID, checksum string) (*Document, error)
	// GetDocumentForDelete loads a document's origin scoped to tenantID, filtering soft-deleted. A
	// miss → ErrDocumentNotFound (→ 404).
	GetDocumentForDelete(ctx context.Context, tx database.Tx, id, tenantID string) (*DocumentForDelete, error)
	// SoftDelete stamps deleted_at (guarded on deleted_at IS NULL) scoped to tenantID. A no-match
	// (already deleted / gone) → ErrDocumentNotFound.
	SoftDelete(ctx context.Context, tx database.Tx, id, tenantID string, deletedAt time.Time) error
}

// pgRepository is the sqlc-backed Repository. Every method binds the generated code to the
// caller's tx; the repo is stateless (nothing to inject at construction).
type pgRepository struct{}

var _ Repository = (*pgRepository)(nil)

// NewRepository returns the Repository. It is stateless: each method binds documentdb to the tx
// it is given, so there is nothing to inject at construction.
func NewRepository() Repository { return &pgRepository{} }

// EnsureCourtRecordInTenant confirms the parent court_record exists in the tenant inside the
// caller's tx. A miss — or a foreign tenant's record — yields pgx.ErrNoRows, mapped to the typed
// ErrCourtRecordNotFound (→ 404) so a document is never grafted onto a non-existent or foreign
// process.
func (r *pgRepository) EnsureCourtRecordInTenant(ctx context.Context, tx database.Tx, tenantID, courtRecordID string) error {
	crid, err := parseUUID(courtRecordID)
	if err != nil {
		return err
	}
	tenant, err := parseUUID(tenantID)
	if err != nil {
		return err
	}

	_, err = documentdb.New(tx).EnsureCourtRecordInTenant(ctx, documentdb.EnsureCourtRecordInTenantParams{
		ID:       crid,
		TenantID: tenant,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrCourtRecordNotFound
	}
	if err != nil {
		return database.WrapInfra(err)
	}
	return nil
}

// InsertDocument persists the new UPLOAD document inside the caller's tx and returns it with its
// DB-assigned id + created_at (echoing the entity, like the deadline InsertDeadline). tenant_id
// is NOT NULL; court_record_id is optional (an avulsa upload). storage_key is set at creation
// (we need it to presign). The mapper lifts the nullable columns.
func (r *pgRepository) InsertDocument(ctx context.Context, tx database.Tx, d *Document) (*Document, error) {
	tenant, err := parseUUID(d.TenantID)
	if err != nil {
		return nil, err
	}
	courtRecordID, err := pgOptionalUUID(d.CourtRecordID)
	if err != nil {
		return nil, err
	}

	row, err := documentdb.New(tx).InsertDocument(ctx, documentdb.InsertDocumentParams{
		TenantID:         tenant,
		CourtRecordID:    courtRecordID,
		DocumentType:     d.DocumentType,
		Origin:           string(d.Origin),
		StorageKey:       textToNull(d.StorageKey),
		Status:           string(d.Status),
		MimeType:         textToNull(d.MimeType),
		SizeBytes:        int64ToNull(d.SizeBytes),
		Title:            textToNull(d.Title),
		OriginalFilename: textToNull(d.OriginalFilename),
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return documentFromInsertRow(row), nil
}

// GetDocumentForComplete loads a document's upload state by id inside the caller's tx, filtered
// by tenantID (barrier 1) and deleted_at IS NULL. A missing id — or one in another tenant / soft-
// deleted — maps to the typed ErrDocumentNotFound (never nil, nil). The mapper lifts the nullable
// court_record_id/mime_type to "".
func (r *pgRepository) GetDocumentForComplete(ctx context.Context, tx database.Tx, id, tenantID string) (*DocumentForComplete, error) {
	did, err := parseUUID(id)
	if err != nil {
		return nil, err
	}
	tenant, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}

	row, err := documentdb.New(tx).GetDocumentForComplete(ctx, documentdb.GetDocumentForCompleteParams{ID: did, TenantID: tenant})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrDocumentNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}

	return &DocumentForComplete{
		ID:            row.ID.String(),
		TenantID:      row.TenantID.String(),
		CourtRecordID: uuidText(row.CourtRecordID),
		Status:        Status(row.Status),
		StorageKey:    derefString(row.StorageKey),
		MimeType:      derefString(row.MimeType),
	}, nil
}

// MarkUploaded flips the document PENDING→UPLOADED inside the caller's tx, keyed by id and
// filtered by tenantID (barrier 1). The query's status='PENDING' guard defends the write against
// a racing complete: a no-match (already transitioned) yields pgx.ErrNoRows, mapped to the typed
// ErrDocumentNotFound. On a hit it returns the full uploaded document (from RETURNING) so
// document.uploaded commits with it in the same tx.
func (r *pgRepository) MarkUploaded(ctx context.Context, tx database.Tx, id, tenantID, checksum string) (*Document, error) {
	did, err := parseUUID(id)
	if err != nil {
		return nil, err
	}
	tenant, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}

	row, err := documentdb.New(tx).MarkDocumentUploaded(ctx, documentdb.MarkDocumentUploadedParams{
		ID:       did,
		TenantID: tenant,
		Checksum: textToNull(checksum),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrDocumentNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return documentFromUploadedRow(row), nil
}

// GetDocumentForDelete loads a document's origin by id inside the caller's tx, filtered by
// tenantID (barrier 1) and deleted_at IS NULL. A missing id — or one in another tenant / already
// deleted — maps to the typed ErrDocumentNotFound.
func (r *pgRepository) GetDocumentForDelete(ctx context.Context, tx database.Tx, id, tenantID string) (*DocumentForDelete, error) {
	did, err := parseUUID(id)
	if err != nil {
		return nil, err
	}
	tenant, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}

	row, err := documentdb.New(tx).GetDocumentForDelete(ctx, documentdb.GetDocumentForDeleteParams{ID: did, TenantID: tenant})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrDocumentNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}

	return &DocumentForDelete{ID: row.ID.String(), Origin: Origin(row.Origin)}, nil
}

// SoftDelete stamps deleted_at inside the caller's tx, keyed by id and filtered by tenantID
// (barrier 1). The query's deleted_at IS NULL guard makes it idempotent: a re-delete touches no
// row, yielding pgx.ErrNoRows, mapped to the typed ErrDocumentNotFound (never a silent 204 on
// nothing).
func (r *pgRepository) SoftDelete(ctx context.Context, tx database.Tx, id, tenantID string, deletedAt time.Time) error {
	did, err := parseUUID(id)
	if err != nil {
		return err
	}
	tenant, err := parseUUID(tenantID)
	if err != nil {
		return err
	}

	_, err = documentdb.New(tx).SoftDeleteDocument(ctx, documentdb.SoftDeleteDocumentParams{
		ID:        did,
		TenantID:  tenant,
		DeletedAt: pgTimestamptz(deletedAt),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrDocumentNotFound
	}
	if err != nil {
		return database.WrapInfra(err)
	}
	return nil
}

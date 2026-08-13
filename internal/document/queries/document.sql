-- document slice queries (the Documentos WRITE path: upload → complete → delete). Every
-- write runs inside the use case's transaction so RLS scopes it to the principal's tenant
-- (barrier 2) on top of the explicit tenant filter (barrier 1). Absence is a typed error
-- at the mapper, never (nil, nil).

-- name: EnsureCourtRecordInTenant :one
-- Guard the upload start: when the request carries a court_record_id, confirm it resolves
-- in the tenant BEFORE creating the document (else a phantom document hangs on a foreign /
-- unknown process). A miss → pgx.ErrNoRows → typed ErrCourtRecordNotFound (→ 404) at the
-- mapper. $1 = id, $2 = tenant_id (both from the trusted principal's request context).
SELECT id
FROM court_record
WHERE id = $1 AND tenant_id = $2;

-- name: InsertDocument :one
-- Start an upload (POST /v1/documentos): persist the document row BORN PENDING, origin
-- UPLOAD, with the storage_key set at creation (we need it to presign the PUT). court_record_id
-- is nullable (an avulsa upload hangs on no process); title/mime_type/size_bytes/original_filename
-- come from the request. checksum/error/deleted_at/pages/extracted_at/extractor_version stay
-- NULL (no bytes yet). tenant_id is NOT NULL. Returns the whole row so the handler renders the
-- presign response + the use case has the created state. $1.. are the columns.
INSERT INTO document (
    tenant_id, court_record_id, document_type, origin,
    storage_key, status, mime_type, size_bytes, title, original_filename
) VALUES (
    $1, $2, $3, $4,
    $5, $6, $7, $8, $9, $10
)
RETURNING id, tenant_id, court_record_id, document_type, origin, storage_key,
          pages, has_text_layer, mime_type, size_bytes, checksum, title,
          original_filename, status, created_at;

-- name: GetDocumentForComplete :one
-- Load a document's upload state — the complete step (POST /v1/documentos/:id/complete)
-- reads it BEFORE the transition so it can gate on PENDING and confirm the object landed.
-- Keyed by id and scoped to tenant_id (barrier 1, on top of RLS barrier 2), filtering
-- deleted_at IS NULL (a soft-deleted document is gone). A miss → pgx.ErrNoRows → typed
-- ErrDocumentNotFound (→ 404) at the mapper. $1 = id, $2 = tenant_id (from the principal).
SELECT id, tenant_id, court_record_id, status, storage_key, mime_type
FROM document
WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL;

-- name: MarkDocumentUploaded :one
-- Confirm the bytes landed (POST /v1/documentos/:id/complete): flip the document
-- PENDING→UPLOADED and set checksum (nullable — the client may omit it), keyed by id and
-- scoped to tenant_id (barrier 1). The `status = 'PENDING'` guard makes the flip SAFE and
-- IDEMPOTENT under concurrency — the caller pre-checks the transition, and this guard defends
-- the write against a racing complete; a no-match (already transitioned) → pgx.ErrNoRows →
-- typed ErrDocumentNotFound at the mapper. Returns the whole row so document.uploaded commits
-- with it in the SAME tx and the handler renders the view. $1 = id, $2 = tenant_id, $3 = checksum.
UPDATE document
SET status = 'UPLOADED', checksum = $3
WHERE id = $1 AND tenant_id = $2 AND status = 'PENDING' AND deleted_at IS NULL
RETURNING id, tenant_id, court_record_id, document_type, origin, storage_key,
          pages, has_text_layer, mime_type, size_bytes, checksum, title,
          original_filename, status, created_at;

-- name: GetDocumentForDelete :one
-- Load a document's origin — the delete (DELETE /v1/documentos/:id) reads it BEFORE the
-- soft delete so it can refuse an origin=COURT document (dos autos, nunca apagável). Keyed
-- by id and scoped to tenant_id (barrier 1), filtering deleted_at IS NULL (a re-delete of an
-- already-gone document is a 404). A miss → pgx.ErrNoRows → typed ErrDocumentNotFound at the
-- mapper. $1 = id, $2 = tenant_id (from the principal).
SELECT id, origin
FROM document
WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL;

-- name: SoftDeleteDocument :one
-- Soft-delete an UPLOAD document (DELETE /v1/documentos/:id): stamp deleted_at = now() (por
-- auditoria — a linha permanece), keyed by id and scoped to tenant_id (barrier 1). The
-- deleted_at IS NULL guard makes it idempotent — a re-delete touches no row → pgx.ErrNoRows →
-- typed ErrDocumentNotFound at the mapper (never a silent 204 on nothing). The use case has
-- already refused origin=COURT. Returns the id. $1 = id, $2 = tenant_id, $3 = deleted_at.
UPDATE document
SET deleted_at = $3
WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL
RETURNING id;

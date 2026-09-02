-- read-model queries (document slice) — the Documentos SCREEN reads, kept OFF the write
-- path (docs: "leitura de tela usa read model, DTO por query dedicada"). Each is tenant-scoped
-- (barrier 1: an explicit tenant_id filter from the trusted principal, never the body) and,
-- where paginated, keyset-paginated on a stable (created_at, id) pair — DESCENDING, since the
-- aba lists newest-first. The caller passes a max sentinel cursor for the first page, so there
-- is no conditional WHERE. Every list filters deleted_at IS NULL (a soft-deleted document is
-- gone from the tela).

-- name: ListDocumentsByProcesso :many
-- The Documentos tab of one process (GET /v1/processos/:id/documentos): the documents anchored
-- on the court_record, newest first. @court_record_id is the court_record id (the same id
-- /processos returns). Descending keyset on (created_at, id); the first page passes the max
-- sentinel. deleted_at IS NULL excludes soft-deleted documents.
SELECT d.id, d.court_record_id, d.document_type, d.origin, d.title,
       d.original_filename, d.mime_type, d.size_bytes, d.pages, d.status,
       d.has_text_layer, d.checksum, d.created_at
FROM document d
WHERE d.court_record_id = @court_record_id::uuid
  AND d.tenant_id = @tenant_id::uuid
  AND d.deleted_at IS NULL
  AND (d.created_at, d.id) < (@last_created::timestamptz, @last_id::uuid)
ORDER BY d.created_at DESC, d.id DESC
LIMIT @page_limit;

-- name: CountDocumentsByProcesso :one
-- The "X de Y" total for the Documentos tab: how many live documents the process holds. Same
-- tenant + court_record scoping (and deleted_at IS NULL filter) as the list.
SELECT count(*) FROM document d
WHERE d.court_record_id = @court_record_id::uuid
  AND d.tenant_id = @tenant_id::uuid
  AND d.deleted_at IS NULL;

-- name: GetDocument :one
-- The detail view of one document (GET /v1/documentos/:id): every field the DocumentView
-- carries. Tenant-scoped (barrier 1) and filtering deleted_at IS NULL: a foreign / unknown /
-- soft-deleted id resolves to no row → pgx.ErrNoRows → typed ErrDocumentNotFound (404) at the
-- repo, never (nil, nil). $1 = id, $2 = tenant_id (from the principal).
SELECT d.id, d.court_record_id, d.document_type, d.origin, d.title,
       d.original_filename, d.mime_type, d.size_bytes, d.pages, d.status,
       d.has_text_layer, d.checksum, d.created_at
FROM document d
WHERE d.id = @id::uuid AND d.tenant_id = @tenant_id::uuid AND d.deleted_at IS NULL;

-- name: GetDocumentChunks :many
-- The extracted text of one document (GET /v1/documentos/:id/content), one row per page. The
-- chunk table carries no tenant_id, so barrier 1 is enforced via an EXISTS subselect on the
-- owning document (same tenant + deleted_at IS NULL scoping as the detail read). Ordered by
-- (page, id) so the caller concatenates the pages in reading order. An empty result means either
-- an unknown/foreign/soft-deleted document OR a live document not yet extracted — the use case
-- disambiguates via GetDocument.
SELECT c.text
FROM chunk c
WHERE c.document_id = @id::uuid
  AND EXISTS (
    SELECT 1 FROM document d
    WHERE d.id = c.document_id
      AND d.tenant_id = @tenant_id::uuid
      AND d.deleted_at IS NULL
  )
ORDER BY c.page, c.id;

package document

import (
	"context"
	"errors"

	validation "github.com/go-ozzo/ozzo-validation/v4"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/httpx"
)

// handler.go is the document slice's HTTP surface — the Documentos milestone Fatia 1: the
// presigned upload (POST /documentos → POST /documentos/:id/complete), the download
// (GET /documentos/:id/download), the aba (GET /processos/:id/documentos), the detail
// (GET /documentos/:id) and the soft delete (DELETE /documentos/:id). The slice owns its
// routing; cmd/api only composes by calling RegisterV1. tenant_id ALWAYS comes from the
// verified principal, never the path/query/body.

// reader is the narrow port the Handler uses from the read use case — the keyset-paginated aba
// read plus the single-document detail.
type reader interface {
	DocumentsByProcesso(ctx context.Context, q DocumentsByProcessoQuery) (DocumentsByProcessoResult, error)
	Document(ctx context.Context, tenantID, id string) (DocumentView, error)
}

// writer is the narrow port the Handler uses from the write use case — the upload start /
// complete, the download presign and the soft delete. It is deliberately separate from reader:
// the writes run on the transactional write path, the reads on the pool.
type writer interface {
	Start(ctx context.Context, cmd StartUploadCommand) (StartUploadResult, error)
	Complete(ctx context.Context, cmd CompleteCommand) (DocumentView, error)
	Download(ctx context.Context, tenantID, documentID string) (DownloadResult, error)
	Delete(ctx context.Context, tenantID, documentID string) error
}

// Handler is the document HTTP surface. It owns its routing; the api only composes by calling
// RegisterV1.
type Handler struct {
	reader reader
	writer writer
}

// NewHandler wires the handler to the read use case (the aba + detail reads) and the write use
// case (upload/download/delete). Both are injected as narrow ports so the binary composes them
// and tests substitute fakes.
func NewHandler(reader reader, writer writer) *Handler {
	return &Handler{reader: reader, writer: writer}
}

// RegisterV1 mounts the document routes on the /v1 group. The static-vs-param ordering matters
// under /documentos/:id: the sub-resource routes (/complete, /download) are declared with their
// own suffix so Fiber never captures them as an :id.
func (h *Handler) RegisterV1(r fiber.Router) {
	r.Post("/documentos", h.startUpload)
	r.Post("/documentos/:id/complete", h.completeUpload)
	r.Get("/documentos/:id/download", h.downloadDocument)
	r.Get("/processos/:id/documentos", h.listDocumentsByProcesso)
	r.Get("/documentos/:id", h.getDocument)
	r.Delete("/documentos/:id", h.deleteDocument)
}

// maxSentinel is the descending keyset's first-page cursor: a created_at above every real row
// and the max uuid, so the newest-first scan starts at the top.
const (
	maxDate = "9999-12-31T23:59:59.999999Z"
	maxUUID = "ffffffff-ffff-ffff-ffff-ffffffffffff"
)

// listDocumentsByProcesso handles GET /v1/processos/:id/documentos: one process's Documentos
// aba — its live documents newest-first, keyset paginated (?limit, ?cursor). The :id is the
// court_record id (the same id /processos returns); tenant_id comes from the principal, and the
// read is tenant-scoped so a foreign :id yields an empty page.
func (h *Handler) listDocumentsByProcesso(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	limit := httpx.ClampLimit(c.QueryInt("limit"), httpx.DefaultLimit, httpx.MaxLimit)

	lastCreated, lastID := maxDate, maxUUID
	if tok := c.Query("cursor"); tok != "" {
		cur, err := httpx.DecodeCursor(tok)
		if err != nil {
			return httpx.WriteError(c, err)
		}
		lastCreated, lastID = cur.LastSortValue, cur.LastID
	}

	created, err := parseCursorTime(lastCreated)
	if err != nil {
		return httpx.WriteError(c, err)
	}

	res, err := h.reader.DocumentsByProcesso(c.UserContext(), DocumentsByProcessoQuery{
		TenantID: tenantID, CourtRecordID: c.Params("id"),
		LastCreated: created, LastID: lastID, Limit: limit,
	})
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(newDocumentsByProcessoPage(res, limit))
}

// getDocument handles GET /v1/documentos/:id: one document's detail. A miss (or a foreign /
// soft-deleted id) is the repo's typed ErrDocumentNotFound → 404. The view is the whole payload,
// so it is returned without a list envelope.
func (h *Handler) getDocument(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	view, err := h.reader.Document(c.UserContext(), tenantID, c.Params("id"))
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(view)
}

// startUpload handles POST /v1/documentos: start an upload. It validates the body, then creates
// the PENDING document and presigns a PUT, returning 201 with {document_id, upload_url,
// storage_key, expires_in}. tenant_id comes from the verified principal, never the body. A bad
// body is a 400 with the {kind,message,details} envelope; a foreign court_record_id is
// ErrCourtRecordNotFound → 404.
func (h *Handler) startUpload(c *fiber.Ctx) error {
	var req startUploadRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteError(c, apperr.NewInvalid("malformed request body"))
	}
	if err := req.Validate(); err != nil {
		return httpx.WriteValidationError(c, err)
	}

	tenantID := httpx.TenantFromCtx(c)
	res, err := h.writer.Start(c.UserContext(), req.toCommand(tenantID))
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(newStartUploadResponse(res))
}

// completeUpload handles POST /v1/documentos/:id/complete: confirm the bytes landed. It parses
// the optional {checksum} body, then flips PENDING→UPLOADED and emits document.uploaded,
// returning 200 with the DocumentView. tenant_id comes from the principal, the id from the path.
// A miss is ErrDocumentNotFound → 404; a non-PENDING document is ErrDocumentNotUploadable → 409;
// absent bytes are ErrDocumentBytesMissing → 409.
func (h *Handler) completeUpload(c *fiber.Ctx) error {
	var req completeUploadRequest
	// The body is optional (checksum only); an empty body is fine, so a parse error is only a
	// 400 when a body was sent and is malformed.
	if len(c.Body()) > 0 {
		if err := c.BodyParser(&req); err != nil {
			return httpx.WriteError(c, apperr.NewInvalid("malformed request body"))
		}
	}

	tenantID := httpx.TenantFromCtx(c)
	view, err := h.writer.Complete(c.UserContext(), CompleteCommand{
		TenantID:   tenantID,
		DocumentID: c.Params("id"),
		Checksum:   req.Checksum,
	})
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(view)
}

// downloadDocument handles GET /v1/documentos/:id/download: presign a GET for the object,
// returning 200 with {url, expires_in}. tenant_id comes from the principal, the id from the
// path. A miss is ErrDocumentNotFound → 404; a document with no storage_key (still PENDING) is
// ErrDocumentNoStorageKey → 409.
func (h *Handler) downloadDocument(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	res, err := h.writer.Download(c.UserContext(), tenantID, c.Params("id"))
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(newDownloadResponse(res))
}

// deleteDocument handles DELETE /v1/documentos/:id: soft-delete an UPLOAD document. No body —
// the id comes from the path, tenant_id from the principal. A miss is ErrDocumentNotFound → 404;
// an origin=COURT document is ErrDocumentNotDeletable → 409; a successful delete is 204 No
// Content.
func (h *Handler) deleteDocument(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	if err := h.writer.Delete(c.UserContext(), tenantID, c.Params("id")); err != nil {
		return httpx.WriteError(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// startUploadRequest is the POST /v1/documentos body: the file's metadata the presign + the
// PENDING row need. tenant_id is NOT here — it comes from the verified principal. court_record_id
// is optional (an avulsa upload); title defaults to original_filename in the use case.
type startUploadRequest struct {
	CourtRecordID    string `json:"court_record_id"`
	DocumentType     string `json:"document_type"`
	Title            string `json:"title"`
	OriginalFilename string `json:"original_filename"`
	MimeType         string `json:"mime_type"`
	SizeBytes        int64  `json:"size_bytes"`
}

// maxUploadBytes is the 50 MB limit on a single document upload (§4e.4 Fatia 2).
const maxUploadBytes = int64(52428800)

// Validate enforces the edge rules: document_type/original_filename/mime_type non-empty, a
// positive size ≤ 50 MB, and (when present) a well-formed court_record_id uuid. A failure is a
// 422 at the edge (KindInvalid) via httpx.WriteValidationError.
func (r startUploadRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.DocumentType, validation.Required),
		validation.Field(&r.OriginalFilename, validation.Required),
		validation.Field(&r.MimeType, validation.Required),
		validation.Field(&r.SizeBytes,
			validation.Required,
			validation.Min(int64(1)),
			validation.Max(maxUploadBytes),
		),
		validation.Field(&r.CourtRecordID, validation.By(uuidIfPresent)),
	)
}

// toCommand maps the validated request + the principal's tenant into the use-case command.
// TenantID comes from the principal (never the body).
func (r startUploadRequest) toCommand(tenantID string) StartUploadCommand {
	return StartUploadCommand{
		TenantID:         tenantID,
		CourtRecordID:    r.CourtRecordID,
		DocumentType:     r.DocumentType,
		Title:            r.Title,
		OriginalFilename: r.OriginalFilename,
		MimeType:         r.MimeType,
		SizeBytes:        r.SizeBytes,
	}
}

// completeUploadRequest is the POST /v1/documentos/:id/complete body: the optional sha256 the
// client computed over the uploaded bytes. Everything else (tenant, id) comes from the principal
// + path.
type completeUploadRequest struct {
	Checksum string `json:"checksum"`
}

// uuidIfPresent rejects a PRESENT, non-empty, malformed uuid; an empty value is a no-op (an
// absent optional court_record_id → an avulsa upload).
func uuidIfPresent(value any) error {
	s, _ := value.(string)
	if s == "" {
		return nil
	}
	if _, err := uuid.Parse(s); err != nil {
		return errors.New("must be a valid uuid")
	}
	return nil
}

// startUploadResponse is the POST /v1/documentos payload (201): the created document's id, the
// presigned PUT URL, the storage key (echoed for correlation) and the TTL in seconds.
type startUploadResponse struct {
	DocumentID string `json:"document_id"`
	UploadURL  string `json:"upload_url"`
	StorageKey string `json:"storage_key"`
	ExpiresIn  int    `json:"expires_in"`
}

// newStartUploadResponse maps the use case's StartUploadResult to the client-facing payload.
func newStartUploadResponse(res StartUploadResult) startUploadResponse {
	return startUploadResponse{
		DocumentID: res.DocumentID,
		UploadURL:  res.UploadURL,
		StorageKey: res.StorageKey,
		ExpiresIn:  res.ExpiresIn,
	}
}

// downloadResponse is the GET /v1/documentos/:id/download payload (200): the presigned GET URL
// and its TTL in seconds.
type downloadResponse struct {
	URL       string `json:"url"`
	ExpiresIn int    `json:"expires_in"`
}

// newDownloadResponse maps the use case's DownloadResult to the client-facing payload.
func newDownloadResponse(res DownloadResult) downloadResponse {
	return downloadResponse{URL: res.URL, ExpiresIn: res.ExpiresIn}
}

// newDocumentsByProcessoPage wraps the Documentos-aba read model in the cursor envelope; the
// next cursor keys off the last row's (created_at, id). There is no filter on the aba, so the "X
// de Y" totals coincide (both the process's live-document count).
func newDocumentsByProcessoPage(res DocumentsByProcessoResult, limit int) httpx.Page[DocumentView] {
	items := res.Items
	if items == nil {
		items = []DocumentView{}
	}
	meta := httpx.PageMeta{Limit: limit, TotalCount: res.Total, Total: res.Total}
	if res.HasMore && len(items) > 0 {
		last := items[len(items)-1]
		tok := httpx.EncodeCursor(httpx.Cursor{
			LastID:        last.ID,
			LastSortValue: last.CreatedAt.UTC().Format(cursorTimeLayout),
		})
		meta.NextCursor = &tok
	}
	return httpx.Page[DocumentView]{Data: items, Page: meta, Filters: res.Filters.NonNil()}
}

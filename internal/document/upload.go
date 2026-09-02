package document

import (
	"context"
	"strings"
	"time"

	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
	"github.com/jusassessoria/platform/lib/storage"
)

// upload.go is the Documentos WRITE path — the 3-step presigned upload (docs/erd-backend.md
// §4e.4) plus the download and the soft delete. Start presigns a PUT and creates the PENDING
// document; Complete confirms the bytes landed (storage.Exists), flips PENDING→UPLOADED and
// emits document.uploaded — the status flip and the outbox row committing together in ONE
// tenant-scoped tx (transactional outbox), mirroring the deadline confirm. Download presigns a
// short-lived GET. Delete soft-deletes an UPLOAD document. tenant_id ALWAYS comes from the
// verified principal, never the path/query/body.

// presignPutTTL is how long an upload URL is valid (§4e.4): 15 minutes is enough for a browser
// PUT of a legal document but short enough that a leaked URL expires fast.
const presignPutTTL = 15 * time.Minute

// presignGetTTL is how long a download URL is valid: 5 minutes — a viewer opens it immediately,
// so a tight window limits exposure of the (tenant-scoped) object.
const presignGetTTL = 5 * time.Minute

// storagePort is the narrow slice of lib/storage the write use case needs: presign a PUT / GET
// for a key and confirm an object exists. Defined consumer-side so a fake substitutes for it in
// tests without touching the network; *storage.Client satisfies it structurally.
type storagePort interface {
	PresignedPut(ctx context.Context, key, contentType string, ttl time.Duration) (string, error)
	PresignedGet(ctx context.Context, key string, ttl time.Duration) (string, error)
	Exists(ctx context.Context, key string) (bool, error)
	// GetBytes reads the object bytes DIRECTLY server-side (no presigned round-trip) — the raw
	// proxy (GET /v1/documentos/:id/raw) needs the actual bytes to stream to the browser.
	GetBytes(ctx context.Context, key string) ([]byte, error)
}

// publisher is the transactional-outbox port — the producer half. *events.Outbox satisfies it
// structurally; Publish writes the document.uploaded row in the same tx as the status flip, so
// the entity and the event commit atomically (transactional outbox).
type publisher interface {
	Publish(ctx context.Context, tx database.Tx, ev events.Event) error
}

// keyGen mints a tenant-scoped storage key at upload start. Defined as a seam so a test injects
// a deterministic key; production wires storage.NewKey.
type keyGen func(tenantID, prefix string) string

// UseCase is the Documentos write use case: it owns the transaction boundary (like the deadline
// confirm), presigns via the storage port, and emits document.uploaded on the outbox. It depends
// only on the ports + the UnitOfWork — never a concrete implementation. now is the clock the soft
// delete stamps deleted_at with; it defaults to time.Now and is overridable in tests via
// WithClock.
type UseCase struct {
	repo    Repository
	storage storagePort
	outbox  publisher
	uow     database.UnitOfWork
	newKey  keyGen
	now     func() time.Time
}

// Option configures a UseCase at construction. Kept as functional options so the clock + key
// seams can be injected in tests without breaking the binary's positional call.
type Option func(*UseCase)

// WithClock overrides the reference clock the soft delete uses to stamp deleted_at. Production
// leaves the default (time.Now); tests pin it to assert the stamped value deterministically.
func WithClock(now func() time.Time) Option {
	return func(uc *UseCase) { uc.now = now }
}

// WithKeyGen overrides the storage-key generator. Production leaves the default
// (storage.NewKey); tests pin it so the presigned key is deterministic.
func WithKeyGen(gen keyGen) Option {
	return func(uc *UseCase) { uc.newKey = gen }
}

// NewUseCase wires the write use case to its repository, storage port, outbox publisher and unit
// of work. opts is variadic so the binary's call stays source-compatible; the clock defaults to
// time.Now and the key generator to storage.NewKey.
func NewUseCase(repo Repository, store storagePort, outbox publisher, uow database.UnitOfWork, opts ...Option) *UseCase {
	uc := &UseCase{repo: repo, storage: store, outbox: outbox, uow: uow, newKey: storage.NewKey, now: time.Now}
	for _, opt := range opts {
		opt(uc)
	}
	return uc
}

// StartUploadCommand is the POST /v1/documentos input the handler builds from the request + the
// verified principal. TenantID comes from the principal, NEVER the body (tenant isolation cannot
// be spoofed). CourtRecordID is optional (an avulsa upload). Title defaults to OriginalFilename
// when absent (filled here at the edge). SizeBytes/MimeType are validated at the edge. Origin
// defaults to OriginUpload (the zero value) — only cmd/worker-court's DocumentWriter adapter sets
// OriginCourt, for documents fetched from the autos rather than uploaded by hand.
type StartUploadCommand struct {
	TenantID         string
	CourtRecordID    string
	DocumentType     string
	Origin           Origin
	Title            string
	OriginalFilename string
	MimeType         string
	SizeBytes        int64
	// CourtEventDate is the eproc event date the document was juntado under — set only
	// by the worker-court DocumentWriter adapter (origin=COURT). A human UPLOAD leaves
	// it zero, which persists as SQL NULL (the column is nullable — only eproc docs
	// carry it).
	CourtEventDate time.Time
}

// StartUploadResult is what Start returns: the created document's id, the presigned PUT URL, the
// storage key (echoed so the client can correlate) and the TTL in seconds.
type StartUploadResult struct {
	DocumentID string
	UploadURL  string
	StorageKey string
	ExpiresIn  int
}

// Start begins an upload (docs/erd-backend.md §4e.4, step 1): in ONE tenant-scoped tx it
// (optionally) verifies the court_record resolves in the tenant, mints a tenant-scoped storage
// key, creates the document BORN PENDING (origin UPLOAD, storage_key set), then presigns a PUT
// for that key + mime_type. The presign is OUTSIDE the tx (it is a stateless AWS-SDK signing, no
// I/O to fail the tx over) but AFTER the insert commits — so a presign that never round-trips to
// a client still leaves a recoverable PENDING row (the client can re-request the URL later; a
// later fatia GCs orphan PENDING rows). TenantID scopes the tx's RLS (barrier 2) + the explicit
// filter (barrier 1); it comes from the verified principal, never the body.
func (uc *UseCase) Start(ctx context.Context, cmd StartUploadCommand) (StartUploadResult, error) {
	title := cmd.Title
	if title == "" {
		title = cmd.OriginalFilename
	}
	origin := cmd.Origin
	if origin == "" {
		origin = OriginUpload
	}
	key := uc.newKey(cmd.TenantID, "documents")

	var created *Document
	err := uc.uow.Do(ctx, cmd.TenantID, func(tx database.Tx) error {
		if cmd.CourtRecordID != "" {
			if err := uc.repo.EnsureCourtRecordInTenant(ctx, tx, cmd.TenantID, cmd.CourtRecordID); err != nil {
				return err
			}
		}
		saved, err := uc.repo.InsertDocument(ctx, tx, &Document{
			TenantID:         cmd.TenantID,
			CourtRecordID:    cmd.CourtRecordID,
			DocumentType:     cmd.DocumentType,
			Origin:           origin,
			StorageKey:       key,
			Status:           StatusPending,
			MimeType:         cmd.MimeType,
			SizeBytes:        cmd.SizeBytes,
			Title:            title,
			OriginalFilename: cmd.OriginalFilename,
			CourtEventDate:   cmd.CourtEventDate,
		})
		if err != nil {
			return err
		}
		created = saved
		return nil
	})
	if err != nil {
		return StartUploadResult{}, err
	}

	url, err := uc.storage.PresignedPut(ctx, created.StorageKey, cmd.MimeType, presignPutTTL)
	if err != nil {
		return StartUploadResult{}, err
	}

	return StartUploadResult{
		DocumentID: created.ID,
		UploadURL:  url,
		StorageKey: created.StorageKey,
		ExpiresIn:  int(presignPutTTL.Seconds()),
	}, nil
}

// CompleteCommand is the POST /v1/documentos/:id/complete input. TenantID + DocumentID come from
// the principal + path; Checksum is optional (the client MAY send the sha256 it computed).
type CompleteCommand struct {
	TenantID   string
	DocumentID string
	Checksum   string
}

// Complete confirms the bytes landed (docs/erd-backend.md §4e.4, step 3): in ONE tenant-scoped
// tx it loads the document (a miss → ErrDocumentNotFound → 404), gates it on PENDING (else
// ErrDocumentNotUploadable → 409), confirms the object is present via storage.Exists (else
// ErrDocumentBytesMissing → 409), flips PENDING→UPLOADED (setting the optional checksum), and
// emits document.uploaded — the flip and the outbox row committing together. The Exists check is
// INSIDE the tx (before the flip) so a document is never marked UPLOADED without its bytes.
// Returns the uploaded DocumentView so the aba reflects the new state without a follow-up read.
func (uc *UseCase) Complete(ctx context.Context, cmd CompleteCommand) (DocumentView, error) {
	var view DocumentView
	err := uc.uow.Do(ctx, cmd.TenantID, func(tx database.Tx) error {
		doc, err := uc.repo.GetDocumentForComplete(ctx, tx, cmd.DocumentID, cmd.TenantID)
		if err != nil {
			return err
		}
		if doc.Status != StatusPending {
			return ErrDocumentNotUploadable
		}
		exists, err := uc.storage.Exists(ctx, doc.StorageKey)
		if err != nil {
			return err
		}
		if !exists {
			return ErrDocumentBytesMissing
		}

		uploaded, err := uc.repo.MarkUploaded(ctx, tx, cmd.DocumentID, cmd.TenantID, cmd.Checksum)
		if err != nil {
			return err
		}
		if err := uc.outbox.Publish(ctx, tx, newDocumentUploaded(uploaded)); err != nil {
			return err
		}
		view = viewFromEntity(uploaded)
		return nil
	})
	if err != nil {
		return DocumentView{}, err
	}
	return view, nil
}

// DownloadResult is what Download returns: the presigned GET URL and its TTL in seconds.
type DownloadResult struct {
	URL       string
	ExpiresIn int
}

// Download presigns a short-lived GET for a document's object (docs/erd-backend.md §4e.4): it
// loads the document tenant-scoped (a miss → ErrDocumentNotFound → 404), refuses a document with
// no storage_key (a PENDING one whose bytes never landed → ErrDocumentNoStorageKey → 409), then
// presigns a GET (TTL 5m). The load runs in a tenant-scoped tx (barrier 1 + 2) so a foreign id
// never resolves; the presign (a stateless signing) runs outside it.
func (uc *UseCase) Download(ctx context.Context, tenantID, documentID string) (DownloadResult, error) {
	var key string
	err := uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		doc, err := uc.repo.GetDocumentForComplete(ctx, tx, documentID, tenantID)
		if err != nil {
			return err
		}
		if doc.StorageKey == "" {
			return ErrDocumentNoStorageKey
		}
		key = doc.StorageKey
		return nil
	})
	if err != nil {
		return DownloadResult{}, err
	}

	url, err := uc.storage.PresignedGet(ctx, key, presignGetTTL)
	if err != nil {
		return DownloadResult{}, err
	}
	return DownloadResult{URL: url, ExpiresIn: int(presignGetTTL.Seconds())}, nil
}

// RawFileResult is what RawFile returns: the object's bytes plus the metadata the handler turns
// into response headers — the ContentType (the document's mime_type, defaulted to
// application/pdf when the column is empty) and the Filename (derived title/original_filename/
// document_type + ".pdf") for the inline Content-Disposition.
type RawFileResult struct {
	Bytes       []byte
	ContentType string
	Filename    string
}

// defaultRawContentType is the Content-Type used when a document has no mime_type recorded — the
// esmagadora maioria dos documentos (dos autos e uploads) é PDF, so this is the safe default for a
// browser viewer.
const defaultRawContentType = "application/pdf"

// RawFile proxies a document's object bytes so the FE can embed it in a viewer (the presigned URL
// points at storage internal/inaccessible to the browser in dev, and we keep the token out of the
// URL). It mirrors Download's resolution — it loads the document tenant-scoped (a miss / foreign /
// soft-deleted id → ErrDocumentNotFound → 404), refuses a document with no storage_key (a PENDING
// one whose bytes never landed → ErrDocumentNoStorageKey → 409) — but instead of presigning it
// reads the bytes DIRECTLY (storage.GetBytes) so the handler streams them. The load runs in a
// tenant-scoped tx (barrier 1 + 2); the byte fetch runs outside it. A storage miss on a resolved
// document is a storage inconsistency (not a 404): the infra error propagates as a 5xx with the
// cause logged, never leaking to the client.
func (uc *UseCase) RawFile(ctx context.Context, tenantID, documentID string) (RawFileResult, error) {
	var meta *DocumentForRaw
	err := uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		doc, err := uc.repo.GetDocumentForRaw(ctx, tx, documentID, tenantID)
		if err != nil {
			return err
		}
		if doc.StorageKey == "" {
			return ErrDocumentNoStorageKey
		}
		meta = doc
		return nil
	})
	if err != nil {
		return RawFileResult{}, err
	}

	bytes, err := uc.storage.GetBytes(ctx, meta.StorageKey)
	if err != nil {
		return RawFileResult{}, err
	}

	// A coluna mime_type às vezes guarda um token curto/inválido (ex.: "pdf" vindo
	// da ingestão do eproc) em vez de um MIME real. Um Content-Type sem "/" não é
	// um media type válido e quebra o viewer do browser (<object type="application/
	// pdf">), então tratamos qualquer valor sem barra como ausente e caímos no PDF.
	contentType := meta.MimeType
	if !strings.Contains(contentType, "/") {
		contentType = defaultRawContentType
	}
	return RawFileResult{
		Bytes:       bytes,
		ContentType: contentType,
		Filename:    rawFilename(meta),
	}, nil
}

// rawFilename derives the inline Content-Disposition filename: the first non-empty of title /
// original_filename / document_type, with a ".pdf" suffix appended when it has no extension (the
// browser viewer keys off the extension). Falls back to "document" so the header is never empty.
func rawFilename(meta *DocumentForRaw) string {
	name := firstNonEmpty(meta.Title, meta.OriginalFilename, meta.DocumentType, "document")
	if !strings.Contains(name, ".") {
		name += ".pdf"
	}
	return name
}

// firstNonEmpty returns the first argument that is not the empty string, or "" when all are empty.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// Delete soft-deletes an UPLOAD document (docs/erd-documentos.md §8): in ONE tenant-scoped tx it
// loads the document (a miss → ErrDocumentNotFound → 404), refuses an origin=COURT document
// (dos autos, nunca apagável → ErrDocumentNotDeletable → 409), then stamps deleted_at = now()
// (the row stays, por auditoria; reads filter deleted_at IS NULL). tenant_id comes from the
// principal, the id from the path.
func (uc *UseCase) Delete(ctx context.Context, tenantID, documentID string) error {
	return uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		doc, err := uc.repo.GetDocumentForDelete(ctx, tx, documentID, tenantID)
		if err != nil {
			return err
		}
		if doc.Origin == OriginCourt {
			return ErrDocumentNotDeletable
		}
		return uc.repo.SoftDelete(ctx, tx, documentID, tenantID, uc.now())
	})
}

// viewFromEntity renders a persisted *Document (the complete write result) as the shared
// DocumentView wire shape — so the write response and the read models are byte-for-byte the same
// document JSON.
func viewFromEntity(d *Document) DocumentView {
	return DocumentView{
		ID:               d.ID,
		CourtRecordID:    d.CourtRecordID,
		DocumentType:     d.DocumentType,
		Origin:           string(d.Origin),
		Title:            d.Title,
		OriginalFilename: d.OriginalFilename,
		MimeType:         d.MimeType,
		SizeBytes:        d.SizeBytes,
		Pages:            d.Pages,
		Status:           string(d.Status),
		HasTextLayer:     d.HasTextLayer,
		Checksum:         d.Checksum,
		CreatedAt:        d.CreatedAt,
	}
}

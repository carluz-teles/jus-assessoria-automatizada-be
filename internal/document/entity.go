// Package document is the Documentos slice (Fatia 1): upload → leitura → aba Documentos.
// A document is a file — either fetched from the autos (origin COURT) or uploaded by the
// escritório (origin UPLOAD). The upload path is a 3-step presigned flow (docs/erd-backend.md
// §4e.4): the client asks the backend for a presigned PUT (POST /documentos), streams the
// bytes straight to storage, then confirms they landed (POST /documentos/:id/complete). The
// bytes never transit the backend.
//
// It is a vertical slice: the write use case opens its own tenant-scoped tx (like the deadline
// confirm), presigns via lib/storage and emits document.uploaded on the transactional outbox;
// the read use case serves the aba off the pool. entity.go holds only the aggregate + value
// types (no repo/lib import).
package document

import "time"

// Document is a file the platform holds for a tenant — the core aggregate. It anchors on a
// court_record (nullable: an avulsa upload hangs on no process) and walks a saga of states
// from PENDING (pre-upload) to READY (indexed). Fatia 1 owns only the front of that saga
// (PENDING→UPLOADED); the extract/chunk/embed stages are later fatias. String ids at the
// domain boundary — the mapper converts uuid↔string like the deadline slice does.
type Document struct {
	ID               string
	TenantID         string
	CourtRecordID    string // "" when the upload hangs on no process (nullable FK)
	DocumentType     string
	Origin           Origin
	StorageKey       string // set at creation (we need it to presign); "" only for a broken row
	Pages            int    // 0 until extraction fills it
	HasTextLayer     bool
	MimeType         string
	SizeBytes        int64
	Checksum         string // "" until complete records it (optional even then)
	Title            string
	OriginalFilename string
	Status           Status
	CreatedAt        time.Time
}

// Status is the document pipeline saga, a closed set the DB CHECK (0033) also enforces. A
// document is BORN PENDING (pre-upload); complete flips it UPLOADED; the extract/chunk/embed
// transitions to EXTRACTING…READY (and the FAILED terminal) are later fatias — Fatia 1 only
// ever writes PENDING (at creation) and UPLOADED (at complete).
type Status string

const (
	StatusPending    Status = "PENDING"
	StatusUploaded   Status = "UPLOADED"
	StatusExtracting Status = "EXTRACTING"
	StatusExtracted  Status = "EXTRACTED"
	StatusChunked    Status = "CHUNKED"
	StatusReady      Status = "READY"
	StatusFailed     Status = "FAILED"
)

// Origin records where the file came from: COURT (baixado dos autos, via MNI/A1 — a later
// fatia) or UPLOAD (enviado pelo escritório, this fatia's write path). The delete path gates
// on it: a COURT document is never deletable (dos autos, por auditoria).
type Origin string

const (
	OriginCourt  Origin = "COURT"
	OriginUpload Origin = "UPLOAD"
)

// DocumentForComplete is the thin state the complete step loads BEFORE the transition
// (GetDocumentForComplete, POST /documentos/:id/complete): the id + tenant (for the outbox
// payload + the guarded flip), the Status the complete is gated on (only PENDING is
// completable), the StorageKey to confirm the object landed (storage.Exists) and echo, the
// CourtRecordID + MimeType the document.uploaded event carries. A miss is ErrDocumentNotFound
// (→ 404), never a zero value.
type DocumentForComplete struct {
	ID            string
	TenantID      string
	CourtRecordID string // "" when the upload hangs on no process
	Status        Status
	StorageKey    string
	MimeType      string
}

// DocumentForDelete is the thin state the delete loads BEFORE the soft delete
// (GetDocumentForDelete, DELETE /documentos/:id): the id + the Origin the delete is gated on
// (origin=COURT is never deletable). A miss is ErrDocumentNotFound (→ 404), never a zero value.
type DocumentForDelete struct {
	ID     string
	Origin Origin
}

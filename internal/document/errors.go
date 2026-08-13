package document

import "github.com/jusassessoria/platform/lib/apperr"

// Typed, HTTP-agnostic slice errors. This slice is an HTTP edge (the Documentos screen),
// so the Kind drives the status code via httpx.WriteError. Absence is always a typed error
// from the repository, never (nil, nil).
var (
	// ErrDocumentNotFound — the requested document id resolves to no LIVE row in the tenant
	// (GET/POST/DELETE /v1/documentos/:id). Typed not-found (→ 404), never (nil, nil): a
	// foreign, unknown, or soft-deleted id is a client-facing miss, not a swallowed empty
	// result.
	ErrDocumentNotFound = apperr.NewNotFound("document not found")

	// ErrDocumentNotUploadable — a POST /v1/documentos/:id/complete on a document that is not
	// PENDING. Complete flips PENDING→UPLOADED only: an already-UPLOADED (or further-along)
	// document cannot be completed again, and a terminal FAILED one is done. CONFLICT (→ 409),
	// distinct from the 404 miss — the document exists, but its state forbids the transition.
	ErrDocumentNotUploadable = apperr.NewConflict("document is not uploadable: only a PENDING document can be completed")

	// ErrDocumentBytesMissing — a POST /v1/documentos/:id/complete where storage.Exists reports
	// the object never landed at the key. The client confirmed too early (the PUT never
	// completed, or targeted the wrong URL). It is a CONFLICT (→ 409): the document exists and
	// is PENDING, but the precondition (bytes present) is not met, so the transition is refused.
	ErrDocumentBytesMissing = apperr.NewConflict("document bytes not found in storage: upload did not complete")

	// ErrDocumentNotDeletable — a DELETE /v1/documentos/:id on an origin=COURT document. A
	// document dos autos is never apagável (por auditoria — só UPLOAD é removível). CONFLICT
	// (→ 409), distinct from the 404 miss — the document exists, but its origin forbids the
	// delete.
	ErrDocumentNotDeletable = apperr.NewConflict("document is not deletable: only an UPLOAD document can be deleted")

	// ErrDocumentNoStorageKey — a GET /v1/documentos/:id/download on a document with no
	// storage_key (a PENDING document whose bytes never landed). There is nothing to presign, so
	// the download is refused. CONFLICT (→ 409): the document exists, but it has no downloadable
	// object yet.
	ErrDocumentNoStorageKey = apperr.NewConflict("document has no storage key: it has not been uploaded yet")

	// ErrCourtRecordNotFound — the request's court_record_id resolves to no row in the tenant
	// (POST /v1/documentos with a court_record_id). Typed not-found (→ 404): a document may not
	// be grafted onto a foreign/unknown process. When no court_record_id is given (an avulsa
	// upload) this guard is skipped.
	ErrCourtRecordNotFound = apperr.NewNotFound("court record not found")
)

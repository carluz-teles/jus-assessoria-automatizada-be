package draft

import "github.com/jusassessoria/platform/lib/apperr"

// Typed, HTTP-agnostic slice errors. Absence is always a typed error from the
// repository, never (nil, nil). The Kind drives the HTTP status at the edge
// (lib/httpx.statusByKind).
var (
	// errGenerationInProgress is returned when POST /v1/pecas/:id/generate is called
	// while the draft is already in EXTRACTING state (generation is already running).
	// CONFLICT (→ 409). Internal sentinel; exported as ErrGenerationInProgress via the
	// generate_trigger.go alias for package-external code that needs to errors.Is it.
	errGenerationInProgress = apperr.NewConflict("generation is already in progress for this draft")

	// ErrAttachmentAlreadyLinked — a POST /v1/pecas/:id/anexos tried to link a
	// document that is already attached to this draft. The UNIQUE (draft_id, document_id)
	// constraint fires. CONFLICT (→ 409).
	ErrAttachmentAlreadyLinked = apperr.NewConflict("document is already attached to this draft")

	// ErrAttachmentNotFound — the requested attachment id resolves to no row in the
	// (draft_id, tenant_id) scope. Typed not-found (→ 404).
	ErrAttachmentNotFound = apperr.NewNotFound("attachment not found")

	// ErrDocumentNotAttachable — a POST /v1/pecas/:id/anexos referred to a document
	// that cannot be attached: either its status is not UPLOADED (still PENDING) or its
	// origin is COURT (dos autos, nunca vinculável a uma peça). INVALID (→ 422).
	ErrDocumentNotAttachable = apperr.NewInvalid("document cannot be attached: it must be origin=UPLOAD and status=UPLOADED")

	// ErrDocumentNotFound is the sentinel for when the document id in the attachment
	// request resolves to no live row in the tenant (wrong tenant, unknown id, or
	// soft-deleted). Typed not-found (→ 404), never (nil, nil).
	ErrDocumentNotFound = apperr.NewNotFound("document not found")

	// ErrDraftNotFound — the requested draft id resolves to no row in the tenant
	// (GET /v1/pecas/:id, PATCH /v1/pecas/:id). Typed not-found (→ 404), never
	// (nil, nil): a foreign or unknown id is a client-facing miss.
	ErrDraftNotFound = apperr.NewNotFound("draft not found")

	// ErrIntimationNotFound — the source=intimation POST supplied an intimation_id
	// that resolves to no row in the tenant (no intimation, or wrong tenant). Typed
	// not-found (→ 404), never (nil, nil).
	ErrIntimationNotFound = apperr.NewNotFound("intimation not found")
)

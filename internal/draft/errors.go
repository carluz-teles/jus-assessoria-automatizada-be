package draft

import "github.com/jusassessoria/platform/lib/apperr"

// Typed, HTTP-agnostic slice errors. Absence is always a typed error from the
// repository, never (nil, nil). The Kind drives the HTTP status at the edge
// (lib/httpx.statusByKind).
var (
	// ErrDraftNotFound — the requested draft id resolves to no row in the tenant
	// (GET /v1/pecas/:id, PATCH /v1/pecas/:id). Typed not-found (→ 404), never
	// (nil, nil): a foreign or unknown id is a client-facing miss.
	ErrDraftNotFound = apperr.NewNotFound("draft not found")

	// ErrIntimationNotFound — the source=intimation POST supplied an intimation_id
	// that resolves to no row in the tenant (no intimation, or wrong tenant). Typed
	// not-found (→ 404), never (nil, nil).
	ErrIntimationNotFound = apperr.NewNotFound("intimation not found")
)

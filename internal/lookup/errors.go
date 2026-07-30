package lookup

import "github.com/jusassessoria/platform/lib/apperr"

// Typed, HTTP-agnostic slice errors. The edge (lib/httpx) maps each apperr.Kind
// to a status; the handler and client only ever see these values. The provider's
// own status codes and payloads are never surfaced — they collapse into these.
var (
	// ErrNotFound — the registry has no record for a well-formed query (the
	// provider answered 404). The edge renders it 404, so the wizard can tell the
	// user "we couldn't find that CNPJ/CEP" instead of showing a failure.
	ErrNotFound = apperr.NewNotFound("registry record not found")

	// ErrInvalidQuery — the provider rejected the query as malformed (a 400).
	// Validation runs before the fetch, so this is a defensive fallback; it maps
	// to 400 like a local validation failure.
	ErrInvalidQuery = apperr.NewInvalid("invalid registry query")
)

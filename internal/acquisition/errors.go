package acquisition

import "github.com/jusassessoria/platform/lib/apperr"

// Typed, HTTP-agnostic slice errors. The edge (lib/httpx) maps each apperr.Kind
// to a status; the domain and repository only ever see these values.
var (
	// ErrIntegrationNotFound — no integration for a (tenant, source). The
	// repository returns it instead of (nil, nil); the activation use case treats
	// it as "first activation", not a client-facing 404.
	ErrIntegrationNotFound = apperr.NewNotFound("integration not found")
)

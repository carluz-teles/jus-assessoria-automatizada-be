package identity

import "github.com/jusassessoria/platform/lib/apperr"

// Typed, HTTP-agnostic slice errors. The edge (lib/httpx) maps each apperr.Kind
// to a status; the domain and repository only ever see these values. Absence is
// a typed error from the repository, never (nil, nil).
var (
	// ErrTenantNotFound — no tenant for the given Clerk org (→ 404).
	ErrTenantNotFound = apperr.NewNotFound("tenant not found")
	// ErrUserNotFound — no app_user for the given Clerk user (→ 404).
	ErrUserNotFound = apperr.NewNotFound("user not found")
	// ErrInvalidRole — a role outside {ADMIN, LAWYER} (→ 400).
	ErrInvalidRole = apperr.NewInvalid("invalid role")
)

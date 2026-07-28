package database

import "github.com/jusassessoria/platform/lib/apperr"

// WrapInfra tags a low-level persistence failure as an infrastructure error so
// the edge (lib/httpx) maps it to a 500 while the original cause travels the
// chain for errors.Is/As and for logging — never for the client (see apperr).
// It is nil-safe: a nil err yields nil so callers can wrap unconditionally.
func WrapInfra(err error) error {
	if err == nil {
		return nil
	}
	return apperr.NewInfra("database error", err)
}

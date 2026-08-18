package eproc

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/jusassessoria/platform/lib/apperr"
)

// statusError is the low-cardinality cause wrapped into an apperr for a bad HTTP status —
// carries the code without putting the (high-cardinality) URL into the error message.
func statusError(code int) error {
	return fmt.Errorf("unexpected status %d", code)
}

// classifyStatus maps a portal response status to a typed apperr, drawing the
// distinctions D0 requires: access denied (Forbidden) ≠ not found (NotFound) ≠ a
// transport/portal fault (Unavailable) ≠ a bad session (Unauthorized). A 2xx is nil.
// The caller has already handled 401/redirect via the re-auth loop before this runs.
func classifyStatus(resp *http.Response) error {
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusUnauthorized:
		return apperr.NewUnauthorized("eproc: unauthorized")
	case resp.StatusCode == http.StatusForbidden:
		return apperr.NewForbidden("eproc: access denied")
	case resp.StatusCode == http.StatusNotFound:
		return apperr.NewNotFound("eproc: process or document not found")
	case resp.StatusCode == http.StatusTooManyRequests:
		return apperr.NewUnavailable("eproc: rate limited", statusError(resp.StatusCode))
	case resp.StatusCode >= 500:
		return apperr.NewUnavailable("eproc: portal error", statusError(resp.StatusCode))
	default:
		return apperr.NewUnavailable("eproc: unexpected status", statusError(resp.StatusCode))
	}
}

// Convenience predicates over the typed errors, so callers (and the dump CLI) can
// branch on the kind without importing apperr's internals directly.

// IsUnauthorized reports a failed authentication (bad/expired credentials).
func IsUnauthorized(err error) bool { return kindIs(err, apperr.KindUnauthorized) }

// IsForbidden reports access denied or a captcha/MFA challenge.
func IsForbidden(err error) bool { return kindIs(err, apperr.KindForbidden) }

// IsNotFound reports a missing process or document (distinct from access denied).
func IsNotFound(err error) bool { return kindIs(err, apperr.KindNotFound) }

// IsUnavailable reports a transport/portal fault the caller may retry.
func IsUnavailable(err error) bool { return kindIs(err, apperr.KindUnavailable) }

// IsInvalid reports a client-side input fault (e.g. missing credentials) — distinct from
// a portal-side rejection, so a caller can tell "you didn't provide creds" from "the
// portal refused them".
func IsInvalid(err error) bool { return kindIs(err, apperr.KindInvalid) }

func kindIs(err error, kind apperr.Kind) bool {
	var ae *apperr.AppError
	if errors.As(err, &ae) {
		return ae.Kind == kind
	}
	return false
}

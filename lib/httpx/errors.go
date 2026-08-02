// Package httpx is the HTTP edge: it is the only place that knows about Fiber
// and HTTP status codes. It maps the transport-agnostic apperr.Kind to a status,
// renders the single client-facing error envelope, and provides the cursor
// pagination primitives. Domain and repositories import lib/apperr, never this.
package httpx

import (
	"errors"
	"log/slog"

	validation "github.com/go-ozzo/ozzo-validation/v4"
	"github.com/gofiber/fiber/v2"

	"github.com/jusassessoria/platform/lib/apperr"
)

// statusByKind is the ONLY place that maps an apperr.Kind to an HTTP status
// (docs/erd-backend.md §4.3). Keeping it here — and nowhere else — is what lets
// the domain and repositories stay free of net/http.
var statusByKind = map[apperr.Kind]int{
	apperr.KindInvalid:      fiber.StatusBadRequest,          // 400
	apperr.KindUnauthorized: fiber.StatusUnauthorized,        // 401
	apperr.KindForbidden:    fiber.StatusForbidden,           // 403
	apperr.KindNotFound:     fiber.StatusNotFound,            // 404
	apperr.KindConflict:     fiber.StatusConflict,            // 409
	apperr.KindInfra:        fiber.StatusInternalServerError, // 500
	apperr.KindUnavailable:  fiber.StatusServiceUnavailable,  // 503
}

// ErrorBody is the single client-facing error envelope (§4.4): {kind, message,
// details?}. It never carries the wrapped cause — that is logged at the edge,
// never leaked to the client.
type ErrorBody struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
}

// WriteError maps any error to the single error envelope and writes it.
//
// A non-AppError is treated as a bug: it is wrapped as INFRA (500) so an
// unclassified error never escapes as anything softer than a server error. For
// status >= 500 the cause is logged at Error with the request context (the
// trace_id rides along) and the client receives a generic message — internals
// never leak. For 400 <= status < 500 the AppError.Message is safe to expose and
// the failure is logged at Warn: 4xx used to vanish silently, so an edge failure
// (a webhook's "invalid signature", a malformed payload, an auth rejection) left
// no trace. Only the log is added — the client-facing envelope is unchanged.
func WriteError(c *fiber.Ctx, err error) error {
	ae, ok := apperr.From(err)
	if !ok {
		// Fiber's router emits its own *fiber.Error for conditions it handles
		// before any of our handlers run — most notably a 404 "Cannot GET /x" for
		// an unmatched route (the steady drip of bots probing /.well-known/*).
		// Honor that status instead of blanketing every non-AppError to INFRA 500:
		// an unmatched route is a client 404, not a server bug, and must not log as
		// "internal error". Only a truly unclassified error is a bug → INFRA 500.
		var fe *fiber.Error
		if errors.As(err, &fe) {
			ae = &apperr.AppError{Kind: kindByStatus(fe.Code), Message: fe.Message}
		} else {
			ae = apperr.NewInfra("internal error", err)
		}
	}

	status, known := statusByKind[ae.Kind]
	if !known {
		status = fiber.StatusInternalServerError
	}

	if status >= fiber.StatusInternalServerError {
		slog.ErrorContext(
			c.UserContext(),
			"internal error",
			"kind", string(ae.Kind),
			"cause", causeOf(ae),
		)

		return c.Status(status).JSON(ErrorBody{
			Kind:    string(ae.Kind),
			Message: "internal error",
		})
	}

	// A 4xx is a client-side failure that was historically silent. Log it at Warn
	// — symmetric with the 5xx branch above — so it leaves a trace with the
	// trace_id riding on the context. causeOf surfaces the wrapped cause (e.g. the
	// svix "invalid signature") when present, or the AppError itself otherwise.
	if status >= fiber.StatusBadRequest {
		slog.WarnContext(
			c.UserContext(),
			"client error",
			"status", status,
			"kind", string(ae.Kind),
			"message", ae.Message,
			"cause", causeOf(ae),
		)
	}

	return c.Status(status).JSON(ErrorBody{
		Kind:    string(ae.Kind),
		Message: ae.Message,
		Details: ae.Details,
	})
}

// WriteValidationError renders ozzo validation failures as a 400 with the
// field→message map under Details. ozzo's validation.Errors is a
// map[string]error that marshals to {field: message} using the request's json
// tags. Anything that is not a validation.Errors falls back to WriteError.
func WriteValidationError(c *fiber.Ctx, err error) error {
	var verrs validation.Errors
	if !errors.As(err, &verrs) {
		return WriteError(c, err)
	}

	return c.Status(fiber.StatusBadRequest).JSON(ErrorBody{
		Kind:    string(apperr.KindInvalid),
		Message: "validation failed",
		Details: verrs,
	})
}

// kindByStatus maps an HTTP status carried by a *fiber.Error (Fiber's built-in
// router/parse failures) back to an apperr.Kind, so it flows through the same
// envelope + log-level policy as a domain error. It is the narrow inverse of
// statusByKind for the statuses Fiber itself produces; anything unlisted collapses
// to Invalid (4xx) or Infra (5xx) by class.
func kindByStatus(status int) apperr.Kind {
	switch status {
	case fiber.StatusNotFound:
		return apperr.KindNotFound
	case fiber.StatusUnauthorized:
		return apperr.KindUnauthorized
	case fiber.StatusForbidden:
		return apperr.KindForbidden
	case fiber.StatusConflict:
		return apperr.KindConflict
	case fiber.StatusServiceUnavailable:
		return apperr.KindUnavailable
	}
	if status >= fiber.StatusInternalServerError {
		return apperr.KindInfra
	}
	return apperr.KindInvalid
}

// causeOf returns the underlying cause the edge logs for a failure (5xx at Error,
// 4xx at Warn). It falls back to the AppError itself when nothing was wrapped, so
// the log line is never empty.
func causeOf(ae *apperr.AppError) error {
	if cause := ae.Unwrap(); cause != nil {
		return cause
	}
	return ae
}

// Package apperr defines the typed, HTTP-agnostic application error core.
//
// It is pure stdlib on purpose: domain entities and infra helpers (e.g.
// lib/database's WrapInfra) import it, so it MUST NOT pull in net/http, Fiber,
// or any other transport. The mapping from Kind to an HTTP status code lives at
// the edge, in lib/httpx — the only place that knows about HTTP.
package apperr

import (
	"errors"
	"fmt"
)

// Kind classifies an AppError independently of any transport. The edge
// (lib/httpx) maps each Kind to an HTTP status; the domain never sees status
// codes. Values match docs/erd-backend.md §4.3.
type Kind string

const (
	KindInvalid      Kind = "DOMAIN_ERROR_INVALID"
	KindUnauthorized Kind = "AUTHENTICATION_ERROR"
	KindForbidden    Kind = "AUTHORIZATION_ERROR"
	KindNotFound     Kind = "ENTITY_NOT_FOUND"
	KindConflict     Kind = "CONFLICT"
	KindInfra        Kind = "INFRA_ERROR"
	// KindUnavailable is a dependency the request needs — a third-party API, an
	// upstream service — being unreachable or failing (→ 503 at the edge). Unlike
	// KindInfra it is not a bug in our code: the caller may simply retry later.
	KindUnavailable Kind = "SERVICE_UNAVAILABLE"
)

// AppError is a typed application error. Message is safe to expose to the
// client; cause is the wrapped underlying error, kept unexported so it never
// leaks across the API boundary — the edge logs it, the client never sees it.
type AppError struct {
	Kind    Kind
	Message string
	Details any
	cause   error // wrapped, never serialized to the client
}

// Error renders Kind and Message, appending the cause when present. This string
// is for logs and developers; the client-facing payload is built at the edge
// from Kind/Message/Details only.
func (e *AppError) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Kind, e.Message, e.cause)
	}
	return fmt.Sprintf("%s: %s", e.Kind, e.Message)
}

// Unwrap exposes the wrapped cause so errors.Is/As traverse the chain.
func (e *AppError) Unwrap() error { return e.cause }

// WithDetails attaches structured details (typically field-level validation
// data for the Invalid case) and returns the receiver for chaining.
func (e *AppError) WithDetails(details any) *AppError {
	e.Details = details
	return e
}

// NewInvalid reports a domain rule or input violation (→ 400 at the edge).
func NewInvalid(msg string) *AppError { return &AppError{Kind: KindInvalid, Message: msg} }

// NewNotFound reports a missing entity (→ 404). Repositories return this
// instead of nil, nil so the absence is typed from the source.
func NewNotFound(msg string) *AppError { return &AppError{Kind: KindNotFound, Message: msg} }

// NewUnauthorized reports a failed authentication (→ 401).
func NewUnauthorized(msg string) *AppError { return &AppError{Kind: KindUnauthorized, Message: msg} }

// NewForbidden reports a failed authorization (→ 403).
func NewForbidden(msg string) *AppError { return &AppError{Kind: KindForbidden, Message: msg} }

// NewConflict reports a state or uniqueness conflict (→ 409).
func NewConflict(msg string) *AppError { return &AppError{Kind: KindConflict, Message: msg} }

// NewInfra wraps an infrastructure failure (→ 500). The cause is preserved for
// errors.Is/As and for logging at the edge, but never exposed to the client.
func NewInfra(msg string, cause error) *AppError {
	return &AppError{Kind: KindInfra, Message: msg, cause: cause}
}

// NewUnavailable reports that an external dependency is unreachable or failing
// (→ 503). The cause is preserved for errors.Is/As and for logging at the edge —
// the client sees only the Kind, never the upstream's raw status or body.
func NewUnavailable(msg string, cause error) *AppError {
	return &AppError{Kind: KindUnavailable, Message: msg, cause: cause}
}

// From extracts an *AppError from anywhere in err's chain. Call sites may also
// use errors.As directly; this is the convenience form.
func From(err error) (*AppError, bool) {
	var ae *AppError
	if errors.As(err, &ae) {
		return ae, true
	}
	return nil, false
}

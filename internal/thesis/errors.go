package thesis

import "github.com/jusassessoria/platform/lib/apperr"

var (
	ErrThesisNotFound       = apperr.NewNotFound("thesis not found")
	ErrDraftSegmentNotFound = apperr.NewNotFound("draft segment not found")
)

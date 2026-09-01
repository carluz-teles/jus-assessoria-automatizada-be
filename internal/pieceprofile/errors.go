package pieceprofile

import "github.com/jusassessoria/platform/lib/apperr"

var (
	ErrPieceProfileNotFound        = apperr.NewNotFound("piece profile not found")
	ErrProfileSectionNotFound      = apperr.NewNotFound("profile section not found")
	ErrPieceProfileVersionNotFound = apperr.NewNotFound("piece profile version not found")
	ErrMatterNotFound              = apperr.NewNotFound("matter not found")
	ErrBaseSkeletonNotFound        = apperr.NewNotFound("base skeleton not found")
	ErrFormatProfileNotFound       = apperr.NewNotFound("format profile not found")
)

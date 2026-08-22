package certificate

import "github.com/jusassessoria/platform/lib/apperr"

// Typed, HTTP-agnostic errors for the slice (lib/apperr). "Not exists" is always
// a typed ENTITY_NOT_FOUND, never (nil, nil). The parse/validation failures are
// KindInvalid so the edge maps them to 4xx — a wrong password or an expired cert
// is a client mistake, not a server fault. None of these carry the password or
// key material.
var (
	// ErrCertificateNotFound — no certificate for that id in the tenant.
	ErrCertificateNotFound = apperr.NewNotFound("certificate not found")

	// ErrInvalidPassword — the .pfx password did not decrypt the file. Message is
	// deliberately generic; it never echoes the attempted password.
	ErrInvalidPassword = apperr.NewInvalid("certificate password is incorrect")

	// ErrMalformedPFX — the upload is not a decodable PKCS#12 file.
	ErrMalformedPFX = apperr.NewInvalid("uploaded file is not a valid .pfx/.p12 certificate")

	// ErrCertificateExpired — the certificate's validity window has already passed.
	ErrCertificateExpired = apperr.NewInvalid("certificate is expired")

	// ErrEmptyFile — the multipart file part was empty.
	ErrEmptyFile = apperr.NewInvalid("certificate file is required")

	// ErrPasswordRequired — the password field was empty.
	ErrPasswordRequired = apperr.NewInvalid("certificate password is required")
)

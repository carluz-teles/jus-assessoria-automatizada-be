package lookup

import (
	"regexp"

	"github.com/jusassessoria/platform/lib/apperr"
)

// Registry identifiers travel in the URL with or without their usual mask
// (e.g. "12.345.678/0001-95" or "01311-902"), so validation normalizes to bare
// digits first, then checks the length. Both patterns are compiled once at
// package level — compilation is O(n) and allocates.
var (
	nonDigit  = regexp.MustCompile(`\D`)
	cnpjExact = regexp.MustCompile(`^\d{14}$`)
	cepExact  = regexp.MustCompile(`^\d{8}$`)
)

// normalizeCNPJ strips any mask and requires exactly 14 digits, returning the
// bare-digit form the provider expects. A bad format is a typed Invalid error
// (→ 400 at the edge) raised BEFORE any network call.
func normalizeCNPJ(raw string) (string, error) {
	digits := nonDigit.ReplaceAllString(raw, "")
	if !cnpjExact.MatchString(digits) {
		return "", apperr.NewInvalid("cnpj must have 14 digits")
	}
	return digits, nil
}

// normalizeCEP strips any mask and requires exactly 8 digits, returning the
// bare-digit form. A bad format is a typed Invalid error (→ 400) before the fetch.
func normalizeCEP(raw string) (string, error) {
	digits := nonDigit.ReplaceAllString(raw, "")
	if !cepExact.MatchString(digits) {
		return "", apperr.NewInvalid("cep must have 8 digits")
	}
	return digits, nil
}

package lookup

import (
	"regexp"

	"github.com/jusassessoria/platform/lib/apperr"
)

// Registry identifiers travel in the URL with or without their usual mask
// (e.g. "12.345.678/0001-95" or "01311-902"), so normalization strips to bare
// digits first. Compiled once at package level — compilation is O(n) and
// allocates.
var (
	nonDigit = regexp.MustCompile(`\D`)
	cepExact = regexp.MustCompile(`^\d{8}$`)
)

// normalizeCNPJ strips any mask, returning the bare-digit form the provider
// expects. No length/format check: the CNPJ field is free text elsewhere in the
// product, so a value that isn't CNPJ-shaped is passed through and simply fails
// (or 404s) at the provider instead of being rejected here.
func normalizeCNPJ(raw string) (string, error) {
	return nonDigit.ReplaceAllString(raw, ""), nil
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

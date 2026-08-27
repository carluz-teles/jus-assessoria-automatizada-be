package identity

import (
	"regexp"
	"strings"

	validation "github.com/go-ozzo/ozzo-validation/v4"
	"github.com/go-ozzo/ozzo-validation/v4/is"
)

// phoneDigits matches a bare phone of 10 or 11 digits (BR landline vs mobile). The
// phone is optional, so ozzo skips this rule on an empty value — only a present
// phone must match. Compiled once at package level. Anchored end-to-end so a value
// carrying a mask or letters fails rather than partially matching.
var phoneDigits = regexp.MustCompile(`^\d{10,11}$`)

// UpdateOrgProfileRequest is the PUT /organization/profile body: the escritório's
// company profile. tenant_id is NOT here — it comes from the verified principal.
// CNPJ is free text: required, but not format-checked (no check-digit algorithm
// exists in the product; the field only records whatever the user typed).
type UpdateOrgProfileRequest struct {
	CNPJ      string  `json:"cnpj"`
	LegalName string  `json:"legal_name"`
	TradeName string  `json:"trade_name"`
	Address   Address `json:"address"`
	// Phone is optional: the escritório's phone (the company's, not the user's).
	// When present it must be 10 or 11 bare digits; empty/absent is valid.
	Phone string `json:"phone"`
	// Email is optional: the escritório's e-mail (the company's, not the user's).
	// When present it must be a well-formed address; empty/absent is valid.
	Email string `json:"email"`
}

// Validate enforces the boundary rules via ozzo (method-based, not struct tags):
// cnpj, legal_name and trade_name are required (cnpj has no format check), and
// phone/email — being optional — are only checked when present (ozzo skips
// non-Required rules on empty values): phone must be 10 or 11 bare digits, email a
// well-formed address. The address is optional AS A WHOLE: when it arrives
// entirely blank (the zero struct) Skip suppresses Address.Validate so a profile
// can be saved with no address; the moment ANY address field is filled, its
// required fields (cidade/uf) apply. A failure here is a 400 at the edge
// (KindInvalid → 400).
func (r UpdateOrgProfileRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.CNPJ, validation.Required),
		validation.Field(&r.LegalName, validation.Required),
		validation.Field(&r.TradeName, validation.Required),
		validation.Field(&r.Address, validation.Skip.When(r.Address == (Address{}))),
		validation.Field(&r.Phone, validation.Match(phoneDigits).Error("must be 10 or 11 digits")),
		validation.Field(&r.Email, is.EmailFormat),
	)
}

// Validate enforces the address rules: only cidade and uf are required — the
// escritório's location. Street fields (cep/logradouro/numero/complemento/bairro)
// are optional and no longer collected by the UI (a postal address isn't used by
// the product), but the struct keeps them so any value already stored survives a
// round-trip. Declaring Validate on Address lets ozzo validate it automatically
// when it is a request field — and it only runs on the write path, never on reads.
func (a Address) Validate() error {
	return validation.ValidateStruct(&a,
		validation.Field(&a.Cidade, validation.Required),
		validation.Field(&a.UF, validation.Required),
	)
}

// toOrgProfile maps the validated request to the use-case input. CNPJ is persisted
// as typed (trimmed only) — no digit-stripping, since the field is no longer
// format-constrained.
func (r UpdateOrgProfileRequest) toOrgProfile() OrgProfile {
	return OrgProfile{
		CNPJ:      strings.TrimSpace(r.CNPJ),
		LegalName: r.LegalName,
		TradeName: r.TradeName,
		Address:   r.Address,
		Phone:     r.Phone,
		Email:     r.Email,
	}
}

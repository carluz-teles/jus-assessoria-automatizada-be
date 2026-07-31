package identity

import (
	"errors"
	"regexp"

	validation "github.com/go-ozzo/ozzo-validation/v4"
	"github.com/go-ozzo/ozzo-validation/v4/is"
)

// nonDigit matches everything that is not a digit, used to strip a CNPJ mask
// (dots, slash, dash, spaces) before validation and persistence. Compiled once at
// package level (compilation is O(n) and allocates).
var nonDigit = regexp.MustCompile(`\D`)

// errCNPJLength is the message for a CNPJ that is not exactly 14 digits once the
// mask is stripped. Lowercase, no trailing punctuation (Go error convention).
var errCNPJLength = errors.New("must be 14 digits")

// UpdateOrgProfileRequest is the PUT /organization/profile body: the escritório's
// company profile. tenant_id is NOT here — it comes from the verified principal.
// CNPJ accepts a masked value ("12.345.678/0001-95"); it is normalized to 14
// digits before persistence (see toOrgProfile).
type UpdateOrgProfileRequest struct {
	CNPJ      string  `json:"cnpj"`
	LegalName string  `json:"legal_name"`
	TradeName string  `json:"trade_name"`
	Address   Address `json:"address"`
}

// SyncRequest is the POST /identity/sync body: the display attributes the JIT
// provisioning needs beyond what the verified token already carries. tenant_id and
// role are NOT here — they come from the token (org id + org role), never the body.
// Only email is required (an app_user needs an address); name and org_name are
// optional display fields the provisioning falls back on.
type SyncRequest struct {
	Email   string `json:"email"`
	Name    string `json:"name"`
	OrgName string `json:"org_name"`
}

// Validate enforces the one boundary rule the token cannot supply: a well-formed
// email (format only — no DNS lookup, which would make the write path flaky). A
// failure here is a 400 at the edge (KindInvalid → 400).
func (r SyncRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.Email, validation.Required, is.EmailFormat),
	)
}

// Validate enforces the boundary rules via ozzo (method-based, not struct tags):
// cnpj must be 14 digits once the mask is stripped, legal_name and trade_name are
// required, and the address must carry its required fields. A failure here is a
// 400 at the edge (KindInvalid → 400).
func (r UpdateOrgProfileRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.CNPJ, validation.Required, validation.By(validCNPJ)),
		validation.Field(&r.LegalName, validation.Required),
		validation.Field(&r.TradeName, validation.Required),
		validation.Field(&r.Address),
	)
}

// Validate enforces the address rules: cep, logradouro, cidade and uf are required
// (numero/complemento/bairro are optional in v0). Declaring Validate on Address
// lets ozzo validate it automatically when it is a request field — and it only
// runs on the write path, never on reads.
func (a Address) Validate() error {
	return validation.ValidateStruct(&a,
		validation.Field(&a.CEP, validation.Required),
		validation.Field(&a.Logradouro, validation.Required),
		validation.Field(&a.Cidade, validation.Required),
		validation.Field(&a.UF, validation.Required),
	)
}

// validCNPJ is the ozzo rule: the value, stripped of its mask, must be exactly 14
// digits. Stripping non-digits means anything left is a digit, so a length check
// is sufficient (a value with letters strips shorter than 14 and fails).
func validCNPJ(value any) error {
	cnpj, _ := value.(string)
	if len(normalizeCNPJ(cnpj)) != 14 {
		return errCNPJLength
	}
	return nil
}

// normalizeCNPJ strips a CNPJ mask down to its bare digits.
func normalizeCNPJ(cnpj string) string {
	return nonDigit.ReplaceAllString(cnpj, "")
}

// toOrgProfile maps the validated request to the use-case input, normalizing the
// CNPJ to 14 digits so what is persisted is mask-free regardless of how it arrived.
func (r UpdateOrgProfileRequest) toOrgProfile() OrgProfile {
	return OrgProfile{
		CNPJ:      normalizeCNPJ(r.CNPJ),
		LegalName: r.LegalName,
		TradeName: r.TradeName,
		Address:   r.Address,
	}
}

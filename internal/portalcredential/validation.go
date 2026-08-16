package portalcredential

import validation "github.com/go-ozzo/ozzo-validation/v4"

// ConfigurePortalCredentialRequest is the PUT /v1/scraping/portal-credential
// body: the advogado's own TJSP eproc login/password. tenant_id and app_user_id
// are NOT here — they come from the verified principal, never the body (CLAUDE.md).
// portal is NOT here either — the route is portal-specific (v0 only knows
// TJSP_EPROC), so there is nothing for the client to choose.
type ConfigurePortalCredentialRequest struct {
	Login    string `json:"login"`
	Password string `json:"password"`
}

// Validate enforces the boundary rule via ozzo: both fields are required and
// non-blank. Whether the pair actually authenticates against the portal is a
// domain concern the use case checks synchronously (domain.go), not a shape
// rule here — a malformed body never even reaches the network.
func (r ConfigurePortalCredentialRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.Login, validation.Required),
		validation.Field(&r.Password, validation.Required),
	)
}

// Package portalcredential is the vertical slice that lets an advogado
// configure, inspect and remove their OWN personal login credential for a court
// portal (v0: TJSP eproc), per docs/erd-tribunal-scraping.md §4.2/§6/§7.
//
// It is deliberately a separate slice from internal/acquisition rather than an
// extension of it: the credential is per-advogado (app_user), not per-tenant
// like `integration` (§4.1 of the ERD walks through why `integration`'s
// UNIQUE(tenant_id, source) cannot hold N credentials). The future scraper
// Connector (a later fatia, inside internal/acquisition) will consume this
// slice's credentials through the PortalCredentialProvider port — a
// cross-slice PORT, never a direct import of this package's entity/repo,
// per CLAUDE.md's "slices only talk through events/ports" rule.
package portalcredential

import "time"

// Portal constants — the court portal a credential is for. TJSPEproc is the
// only value the v0 endpoints accept (validation.go); the column is text, not
// an enum, so a future portal (ESAJ, PJE) is a validation-list change, not a
// migration.
const (
	PortalTJSPEproc = "TJSP_EPROC"
)

// Status constants — a portal_credential's lifecycle, set by the synchronous
// login test the Configure use case runs before persisting (domain.go). ACTIVE
// means the last test logged in successfully; AUTH_FAILED covers BOTH an
// explicit rejection (wrong password) and an inconclusive test (timeout,
// unparseable page) — the UI's "reconfigure" affordance is the same either way,
// the difference lives in LastError's text. CAPTCHA_BLOCKED and DISABLED are
// carried by the schema for a later fatia (the recurring sync connector, and an
// explicit disable action) — Configure never sets them in v0.
const (
	StatusActive         = "ACTIVE"
	StatusAuthFailed     = "AUTH_FAILED"
	StatusCaptchaBlocked = "CAPTCHA_BLOCKED"
	StatusDisabled       = "DISABLED"
)

// secretPurposePortalPassword tags a tenant_secret row as a portal_credential's
// password. It is not persisted (0042's tenant_secret has no purpose column —
// this slice is its sole producer in v0), but documents the intent at the call
// site (repository.go) for whoever adds the next secret purpose later.
const secretPurposePortalPassword = "PORTAL_CREDENTIAL_PASSWORD"

// PortalCredential is one advogado's login for one court portal. Password is
// NEVER a field here — CredentialRef is the pointer to the sealed row in
// tenant_secret; the plaintext exists only transiently inside Configure, for
// the login test and the Seal call, and is never logged, returned, or stored.
type PortalCredential struct {
	ID             string
	TenantID       string
	AppUserID      string
	Portal         string
	Login          string
	CredentialRef  string
	Status         string
	LastError      string
	LastVerifiedAt time.Time // zero when never verified
	ConfiguredBy   string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

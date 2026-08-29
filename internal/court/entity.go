// Package court is the vertical slice for authenticated access to a tribunal portal
// (docs/erd-execucao-judicial-tjsp.md §5). It owns court_connection — the
// authenticated-session state per (advogado, tribunal, sistema) — and the
// CourtProvider abstraction that hides each portal's real login mechanism (mutual
// TLS + Keycloak's X.509 authenticator for eproc today; a future e-SAJ/PJe adapter
// implements the same interface without this slice's callers changing).
//
// Scope of this first fatia: AUTHENTICATION ONLY (Connect + automated MFA
// enrollment) — the blocker the team is attacking today. Fetching autos and
// filing petitions reuse this connection in later fatias (ERD §6/§7); this slice
// does not import document/draft/petition and never will (event-only
// communication, same as every other slice).
package court

import "time"

// AuthenticationMethod is how the lawyer's identity is proven to the portal.
type AuthenticationMethod string

const (
	AuthenticationMethodPassword      AuthenticationMethod = "PASSWORD"
	AuthenticationMethodCertificateA1 AuthenticationMethod = "CERTIFICATE_A1"
)

// Status mirrors court_connection.status — the state machine docs §5 asks for, plus
// MFA_ENROLLMENT_REQUIRED (see below).
type Status string

const (
	StatusDisconnected   Status = "DISCONNECTED"
	StatusAuthenticating Status = "AUTHENTICATING"
	StatusConnected      Status = "CONNECTED"
	// StatusMFAEnrollmentRequired means this connection has never captured a TOTP
	// seed — Connect will try EnrollMFA automatically before giving up here. Seeing
	// this status persisted (rather than transient mid-request) means enrollment
	// itself failed and needs attention — it is NOT the expected steady state for a
	// MFA-required portal (that's StatusConnected, seed already on file).
	StatusMFAEnrollmentRequired Status = "MFA_ENROLLMENT_REQUIRED"
	// StatusMFARequired means a seed IS on file but the provider still hit a
	// challenge it couldn't complete (wrong/expired seed, portal-side hiccup) —
	// distinct from "never enrolled", this signals the STORED seed needs
	// re-capturing, not a first-time enrollment.
	StatusMFARequired         Status = "MFA_REQUIRED"
	StatusCertificateRequired Status = "CERTIFICATE_REQUIRED"
	StatusReauthRequired      Status = "REAUTH_REQUIRED"
	StatusError               Status = "ERROR"
)

// CourtConnection is the aggregate root — one row per (tenant, advogado, tribunal,
// sistema). CredentialRef/CertificateRef/MFASeedRef/SessionRef are vault pointers
// (tenant_secret.id, or certificate.id for CertificateRef) — never the secret
// itself; the use case resolves them right before calling a CourtProvider and lets
// them go out of scope immediately after.
type CourtConnection struct {
	ID                   string
	TenantID             string
	AppUserID            string
	Court                string // "TJSP" (only value today)
	System               string // "EPROC" (only value today)
	AuthenticationMethod AuthenticationMethod
	CredentialRef        string // tenant_secret.id, when AuthenticationMethod == PASSWORD
	CertificateRef       string // certificate.id, when AuthenticationMethod == CERTIFICATE_A1
	MFASeedRef           string // tenant_secret.id, "" until enrolled
	Status               Status
	LastAuthenticatedAt  *time.Time
	Error                string // last failure, human-readable — never a secret
	CreatedAt            time.Time
}

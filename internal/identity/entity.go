// Package identity is the vertical slice for tenants and users — the local
// projection of Clerk Organizations and Users (docs/erd-backend.md §4d). Clerk
// is the source of identity; this slice is the source of authorization and the
// FK target (tenant_id) every other table references.
package identity

import "time"

// Role is the product-level authorization role inside an escritório. It is a
// text enum validated in the application (CHECK-on-app), not a DB enum type.
type Role string

const (
	RoleAdmin  Role = "ADMIN"
	RoleLawyer Role = "LAWYER"
)

// Valid reports whether r is one of the known roles. The zero value ("") is
// invalid on purpose, so an unset role never silently passes as a real one.
func (r Role) Valid() bool {
	return r == RoleAdmin || r == RoleLawyer
}

// Tenant mirrors a Clerk Organization (the escritório). Its ID is the internal
// uuid used by every FK in the system; ClerkOrgID is only the bridge to Clerk.
//
// The company-profile fields (CNPJ, LegalName, TradeName, Address) and the
// onboarding gate are captured during onboarding (fatia B) and left zero until
// then: a tenant is provisioned from the Clerk webhook before its profile is
// filled in. Address is a pointer because the whole jsonb column may be absent;
// OnboardingCompletedAt is nil until the org profile is first saved.
type Tenant struct {
	ID                    string
	ClerkOrgID            string
	Name                  string
	CreatedAt             time.Time
	CNPJ                  string
	LegalName             string
	TradeName             string
	Address               *Address
	OnboardingCompletedAt *time.Time
}

// Address is the escritório's postal address, stored as the tenant.address jsonb
// column and echoed back on the profile response. The json tags are the single
// source of truth for BOTH the jsonb serialization and the API shape.
type Address struct {
	CEP         string `json:"cep"`
	Logradouro  string `json:"logradouro"`
	Numero      string `json:"numero"`
	Complemento string `json:"complemento"`
	Bairro      string `json:"bairro"`
	Cidade      string `json:"cidade"`
	UF          string `json:"uf"`
}

// OrgProfile is the validated company profile a use case persists onto a tenant
// during onboarding (PUT /organization/profile). CNPJ is already normalized to 14
// digits by the time it reaches the use case (the request layer strips the mask).
type OrgProfile struct {
	CNPJ      string
	LegalName string
	TradeName string
	Address   Address
}

// Me is the onboarding read model for GET /identity/me: who the caller is and how
// far along onboarding they are. UserID is the Clerk user id (the stable identity
// available before a tenant exists). TenantID and OnboardingCompletedAt are nil
// when the authenticated user has no tenant yet — that is a valid "not onboarded"
// state, not an error.
type Me struct {
	UserID                string
	TenantID              *string
	OnboardingCompletedAt *time.Time
}

// AppUser mirrors a Clerk User, already linked to its tenant (1 user = 1
// escritório). TenantID is the internal uuid, never the Clerk org id.
type AppUser struct {
	ID          string
	ClerkUserID string
	TenantID    string
	Email       string
	Name        string
	// Phone is the user's phone number, synced from the Clerk User
	// (user.updated). Optional: an empty string stands in for SQL NULL when the
	// user has no phone on Clerk.
	Phone     string
	Role      Role
	CreatedAt time.Time
}

// Principal is the authenticated caller resolved from a verified Clerk JWT: who
// they are (UserID), which escritório they belong to (internal TenantID), and
// what they may do (Role). The auth middleware (a later slice) builds it via
// ResolvePrincipal and injects it into the request context; handlers read the
// tenant from here, never from the request body.
type Principal struct {
	UserID   string
	TenantID string
	Role     Role
}

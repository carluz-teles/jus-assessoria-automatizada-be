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
type Tenant struct {
	ID         string
	ClerkOrgID string
	Name       string
	CreatedAt  time.Time
}

// AppUser mirrors a Clerk User, already linked to its tenant (1 user = 1
// escritório). TenantID is the internal uuid, never the Clerk org id.
type AppUser struct {
	ID          string
	ClerkUserID string
	TenantID    string
	Email       string
	Name        string
	Role        Role
	CreatedAt   time.Time
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

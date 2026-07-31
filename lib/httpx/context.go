package httpx

import "github.com/gofiber/fiber/v2"

// Principal is the authenticated caller resolved from the Clerk JWT by the auth
// middleware (a later slice). Handlers read tenant_id from here — never from the
// request body — so tenant isolation cannot be spoofed by the client.
type Principal struct {
	UserID   string
	TenantID string
	Role     string
}

// principalKey is the request-local key under which the auth middleware stores
// the resolved Principal.
const principalKey = "principal"

// SetPrincipal stores the resolved principal on the request locals. Called by
// the auth middleware once JWT verification and org_id→tenant_id resolution
// succeed.
func SetPrincipal(c *fiber.Ctx, p Principal) {
	c.Locals(principalKey, p)
}

// PrincipalFromCtx returns the principal placed by the auth middleware. The bool
// is false on an unauthenticated request (nothing was set).
func PrincipalFromCtx(c *fiber.Ctx) (Principal, bool) {
	p, ok := c.Locals(principalKey).(Principal)
	return p, ok
}

// clerkUserIDKey is the request-local key under which the tenant-less auth
// middleware (AuthUser) stores the verified Clerk user id. It is deliberately
// separate from principalKey: an AuthUser request has an authenticated caller but
// no resolved Principal (no tenant yet), so it must not masquerade as one.
const clerkUserIDKey = "clerk_user_id"

// SetClerkUserID stores the verified Clerk user id on the request locals. Called
// by AuthUser once JWT verification succeeds, for the onboarding endpoints that
// run before a tenant exists — so there is no Principal to resolve, only proof of
// a signed-in user.
func SetClerkUserID(c *fiber.Ctx, clerkUserID string) {
	c.Locals(clerkUserIDKey, clerkUserID)
}

// ClerkUserIDFromCtx returns the Clerk user id AuthUser placed on the request.
// The bool is false when AuthUser did not run (no tenant-less session marker).
func ClerkUserIDFromCtx(c *fiber.Ctx) (string, bool) {
	id, ok := c.Locals(clerkUserIDKey).(string)
	return id, ok
}

// clerkOrgKey is the request-local key under which the tenant-less auth middleware
// (AuthUser) stores the verified Clerk org id + role. Like clerkUserIDKey it is
// deliberately separate from principalKey: an AuthUser request has a verified org
// context but no resolved Principal (no internal tenant yet), so it must not
// masquerade as one.
const clerkOrgKey = "clerk_org"

// clerkOrg is the verified Clerk organization context AuthUser carries for the
// tenant-less onboarding endpoints that provision just-in-time (POST
// /identity/sync): the external org id and org role, straight from the token,
// before any org_id→tenant_id resolution.
type clerkOrg struct {
	orgID string
	role  string
}

// SetClerkOrg stores the verified Clerk org id and role on the request locals.
// Called by AuthUser alongside SetClerkUserID: the JIT sync endpoint provisions the
// tenant from this org context in the same request, so it needs the org id (and the
// role) the token carries before a tenant exists.
func SetClerkOrg(c *fiber.Ctx, orgID, role string) {
	c.Locals(clerkOrgKey, clerkOrg{orgID: orgID, role: role})
}

// ClerkOrgFromCtx returns the Clerk org id and role AuthUser placed on the request.
// The bool is false when AuthUser did not run OR the token carried no org (a
// signed-in user who belongs to no organization yet): with no org there is nothing
// to provision, so the caller answers 401 instead of provisioning a blank tenant.
func ClerkOrgFromCtx(c *fiber.Ctx) (orgID, role string, ok bool) {
	o, isSet := c.Locals(clerkOrgKey).(clerkOrg)
	if !isSet || o.orgID == "" {
		return "", "", false
	}
	return o.orgID, o.role, true
}

// TenantFromCtx is the convenience accessor for the tenant id that flows into
// every repository call. Returns "" when the request is unauthenticated.
func TenantFromCtx(c *fiber.Ctx) string {
	p, ok := PrincipalFromCtx(c)
	if !ok {
		return ""
	}
	return p.TenantID
}

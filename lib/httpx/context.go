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

// TenantFromCtx is the convenience accessor for the tenant id that flows into
// every repository call. Returns "" when the request is unauthenticated.
func TenantFromCtx(c *fiber.Ctx) string {
	p, ok := PrincipalFromCtx(c)
	if !ok {
		return ""
	}
	return p.TenantID
}

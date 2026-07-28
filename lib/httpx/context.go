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

// TenantFromCtx is the convenience accessor for the tenant id that flows into
// every repository call. Returns "" when the request is unauthenticated.
func TenantFromCtx(c *fiber.Ctx) string {
	p, ok := PrincipalFromCtx(c)
	if !ok {
		return ""
	}
	return p.TenantID
}

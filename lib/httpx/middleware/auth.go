package middleware

import (
	"context"
	"strings"

	"github.com/gofiber/fiber/v2"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/httpx"
)

// TokenVerifier abstracts Clerk JWT verification (docs §4d.3, via 1). It returns
// the external Clerk identity carried by a verified token; the concrete
// ClerkVerifier lives beside this file, and Auth depends only on the interface so
// it can be exercised with a fake in unit tests.
type TokenVerifier interface {
	// Verify validates the bearer JWT (signature, expiry, issuer) and returns the
	// Clerk user id, org id and org role from its claims. A non-nil error means
	// the token is untrusted — the caller answers 401.
	Verify(ctx context.Context, bearer string) (userID, orgID, role string, err error)
}

// PrincipalResolver turns a verified external Clerk identity into the internal
// Principal the rest of the request relies on — TenantID is the internal uuid,
// never the Clerk org id (docs §4d.3). internal/identity implements this; lib
// must not import internal, so the port is declared here and the api binary
// injects the implementation.
type PrincipalResolver interface {
	Resolve(ctx context.Context, clerkUserID, clerkOrgID string) (httpx.Principal, error)
}

const bearerPrefix = "Bearer "

// Auth is the authentication middleware (docs §4d.3/§4d.6): it verifies the
// Clerk JWT, resolves the external identity to an internal Principal and injects
// it so downstream handlers read the tenant from TenantFromCtx, never from the
// request body. It slots after Recover (a panic in verification is caught) and
// before RequestLogger (the access log carries the resolved tenant); see
// WithAuth.
//
// The org role the verifier extracts is intentionally discarded: the database is
// the source of authorization, so the Principal's role comes from Resolve
// (app_user.role), not from the token.
func Auth(v TokenVerifier, r PrincipalResolver) fiber.Handler {
	return func(c *fiber.Ctx) error {
		bearer := bearerToken(c)
		if bearer == "" {
			return httpx.WriteError(c, apperr.NewUnauthorized("missing bearer token"))
		}

		ctx := c.UserContext()

		userID, orgID, _, err := v.Verify(ctx, bearer)
		if err != nil {
			return httpx.WriteError(c, apperr.NewUnauthorized("invalid token"))
		}

		principal, err := r.Resolve(ctx, userID, orgID)
		if err != nil {
			return httpx.WriteError(c, authError(err))
		}

		httpx.SetPrincipal(c, principal)
		return c.Next()
	}
}

// AuthUser is the tenant-less authentication middleware for the onboarding
// endpoints the wizard hits BEFORE an escritório (tenant) exists — e.g. the
// CNPJ/CEP lookups in step 2. It verifies the Clerk JWT with the SAME
// TokenVerifier as Auth, but stops there: it does NOT call Resolve and does NOT
// require a tenant, so a freshly-signed-up user with no org still passes. A
// missing or invalid token is a 401, exactly like Auth; a valid one injects the
// Clerk user id as a session marker (ClerkUserIDFromCtx) and continues.
//
// It shares bearerToken and the 401 semantics with Auth on purpose; the only
// difference is the absent principal resolution.
func AuthUser(v TokenVerifier) fiber.Handler {
	return func(c *fiber.Ctx) error {
		bearer := bearerToken(c)
		if bearer == "" {
			return httpx.WriteError(c, apperr.NewUnauthorized("missing bearer token"))
		}

		userID, _, _, err := v.Verify(c.UserContext(), bearer)
		if err != nil {
			return httpx.WriteError(c, apperr.NewUnauthorized("invalid token"))
		}

		httpx.SetClerkUserID(c, userID)
		return c.Next()
	}
}

// RequireRole guards a route by product role (docs §4d.5). It reads the Principal
// Auth injected and answers 403 when the role does not match. A missing Principal
// means Auth did not run ahead of this guard — that is a 401, not a 403.
func RequireRole(role string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		p, ok := httpx.PrincipalFromCtx(c)
		if !ok {
			return httpx.WriteError(c, apperr.NewUnauthorized("missing principal"))
		}
		if p.Role != role {
			return httpx.WriteError(c, apperr.NewForbidden("insufficient role"))
		}
		return c.Next()
	}
}

// authError maps a resolver failure to the response the auth boundary returns. A
// not-found principal (user/tenant not provisioned yet — the §4d.3 first-login
// race — or a user/org mismatch) is an authentication failure here, not a 404:
// the caller holds a valid token but is not a principal we know, so we answer 401
// and never reveal which half was missing. Infra and other errors propagate
// unchanged (a database outage is a 500, not a 401).
func authError(err error) error {
	if ae, ok := apperr.From(err); ok && ae.Kind == apperr.KindNotFound {
		return apperr.NewUnauthorized("unknown principal")
	}
	return err
}

// bearerToken extracts the token from an "Authorization: Bearer <jwt>" header,
// returning "" when the header is absent or not a bearer credential. The scheme
// match is case-insensitive per RFC 7235.
//
// Fallback pra SSE (EventSource): a spec HTML5 EventSource não permite headers
// custom, então endpoints de stream aceitam ?token=<jwt> na query. Só é lido
// quando o header Authorization está ausente — mantém segurança normal do REST.
func bearerToken(c *fiber.Ctx) string {
	h := c.Get(fiber.HeaderAuthorization)
	if len(h) >= len(bearerPrefix) && strings.EqualFold(h[:len(bearerPrefix)], bearerPrefix) {
		return strings.TrimSpace(h[len(bearerPrefix):])
	}
	// SSE fallback
	if q := strings.TrimSpace(c.Query("token")); q != "" {
		return q
	}
	return ""
}

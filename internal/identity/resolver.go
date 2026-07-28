package identity

import (
	"context"

	"github.com/jusassessoria/platform/lib/httpx"
)

// Resolver adapts the identity UseCase to the auth middleware's PrincipalResolver
// port (middleware.PrincipalResolver). It lives in the slice, not in lib: lib
// must not import internal, so the port is declared at the edge and satisfied
// here structurally — the api binary injects a *Resolver where the middleware
// expects the interface.
type Resolver struct {
	uc *UseCase
}

// NewResolver wires the resolver to the identity use cases.
func NewResolver(uc *UseCase) *Resolver {
	return &Resolver{uc: uc}
}

// Resolve looks up the internal Principal behind a verified Clerk identity and
// maps it to the transport-level httpx.Principal the middleware injects. The
// slice Role (a typed enum) is widened to the string the edge carries.
//
// A not-found principal (the webhook has not provisioned the user yet — the
// §4d.3 first-login race) propagates unchanged; the auth boundary maps it to 401.
// The one-shot Clerk API fallback that lets the very first login win that race is
// PENDING.
func (r *Resolver) Resolve(ctx context.Context, clerkUserID, clerkOrgID string) (httpx.Principal, error) {
	p, err := r.uc.ResolvePrincipal(ctx, clerkUserID, clerkOrgID)
	if err != nil {
		return httpx.Principal{}, err
	}

	return httpx.Principal{
		UserID:   p.UserID,
		TenantID: p.TenantID,
		Role:     string(p.Role),
	}, nil
}

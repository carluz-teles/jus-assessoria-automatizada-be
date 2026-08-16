package identity

import (
	"context"

	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/clerk/clerk-sdk-go/v2/organizationmembership"

	"github.com/jusassessoria/platform/lib/apperr"
)

// clerkMembershipGateway is the concrete membershipGateway backed by the Clerk
// API. It carries no secret of its own: NewClerkVerifier already installs the
// instance secret via clerk.SetKey at boot (lib/httpx/middleware), and
// organizationmembership.NewClient falls back to that package-level key when its
// ClientConfig is zero-valued.
type clerkMembershipGateway struct {
	client *organizationmembership.Client
}

var _ membershipGateway = (*clerkMembershipGateway)(nil)

// NewClerkMembershipGateway builds the gateway over the Clerk Organization
// Memberships API. Call after clerk.SetKey has run (main wires
// NewClerkVerifier first), or requests fail with an empty secret key.
func NewClerkMembershipGateway() *clerkMembershipGateway {
	return &clerkMembershipGateway{
		client: organizationmembership.NewClient(&clerk.ClientConfig{}),
	}
}

// RemoveMember calls DELETE /organizations/{org}/memberships/{user} on Clerk.
// Clerk's Organization Memberships API is keyed by (org id, user id), not by our
// clerk_membership_id — so the caller only needs those two ids, not the
// membership row. Any failure (network, rate limit, Clerk 5xx, an already-gone
// membership) maps to apperr.NewUnavailable — the provider is momentarily
// unreachable or already consistent, and the caller may retry — mirroring
// billing's mapStripeError for the same third-party-gateway shape.
func (g *clerkMembershipGateway) RemoveMember(ctx context.Context, clerkOrgID, clerkUserID string) error {
	_, err := g.client.Delete(ctx, &organizationmembership.DeleteParams{
		OrganizationID: clerkOrgID,
		UserID:         clerkUserID,
	})
	if err != nil {
		return apperr.NewUnavailable("clerk: remove organization member", err)
	}
	return nil
}

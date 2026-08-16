package identity

import "context"

// membershipGateway is the port over Clerk's Organization Memberships API the
// RemoveMember use case needs to revoke access. The domain depends on this
// interface only; the concrete implementation (clerkMembershipGateway, in
// clerk_gateway.go) owns the clerk-sdk-go dependency, so entity.go/domain.go
// never import the SDK (docs §4b: the slice's core stays pure). Mirrors the
// StripeGateway/Channel port pattern already used by billing/notifications.
type membershipGateway interface {
	// RemoveMember asks Clerk to revoke a user's membership in an organization.
	// The local projection (membership ACTIVE→REMOVED, identity.member_removed)
	// is NOT written here — it lands later through the organizationMembership.deleted
	// webhook this same removal triggers (OnMembershipRemoved), keeping Clerk the
	// single source of truth for the membership lifecycle.
	RemoveMember(ctx context.Context, clerkOrgID, clerkUserID string) error
}

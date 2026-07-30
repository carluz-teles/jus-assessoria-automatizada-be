package identity

import "github.com/jusassessoria/platform/lib/events"

// Event contracts for the identity slice. Other slices may import these structs
// as the shape they consume — they are the only identity types allowed to cross
// a slice boundary (slices communicate by event, never by entity/repo).
//
// TenantProvisioned/UserProvisioned are plain shapes not yet published (the
// producer wiring lands with the consumer that needs them). OrgProfileUpdated IS
// published — the onboarding profile write emits it through the outbox.

// TypeOrgProfileUpdated is the dotted id of the event emitted when an escritório's
// company profile is saved during onboarding. Its aggregate is the tenant.
const TypeOrgProfileUpdated = "identity.org_profile_updated"

// TypeMemberJoined is the dotted id of the event emitted when a user joins a
// tenant (an ACTIVE membership is first created or reactivated). Its aggregate is
// the tenant.
const TypeMemberJoined = "identity.member_joined"

const aggregateTypeTenant = "tenant"

// TenantProvisioned is emitted after a tenant is created or synced from Clerk.
type TenantProvisioned struct {
	TenantID   string
	ClerkOrgID string
}

// UserProvisioned is emitted after an app_user is created or synced from Clerk.
type UserProvisioned struct {
	UserID   string
	TenantID string
}

// OrgProfileUpdated is emitted, in the same transaction as the tenant profile
// write, when the escritório's company profile is saved. The payload carries the
// tenant and the just-saved identifying fields (CNPJ, trade name) so a consumer
// can react without re-reading the tenant. Base adds the event id (consumer dedup)
// and the aggregate id (the tenant).
type OrgProfileUpdated struct {
	events.Base
	TenantID  string `json:"tenant_id"`
	CNPJ      string `json:"cnpj"`
	TradeName string `json:"trade_name"`
}

var _ events.Event = OrgProfileUpdated{}

func (OrgProfileUpdated) Type() string          { return TypeOrgProfileUpdated }
func (OrgProfileUpdated) AggregateType() string { return aggregateTypeTenant }

// MemberJoined is emitted, in the same transaction as the membership write, when a
// user joins a tenant (a new or reactivated ACTIVE membership). The payload carries
// the internal ids and the role so a consumer can react without re-reading the
// membership. Base adds the event id (consumer dedup) and the aggregate id (tenant).
type MemberJoined struct {
	events.Base
	TenantID  string `json:"tenant_id"`
	AppUserID string `json:"app_user_id"`
	Role      Role   `json:"role"`
}

var _ events.Event = MemberJoined{}

func (MemberJoined) Type() string          { return TypeMemberJoined }
func (MemberJoined) AggregateType() string { return aggregateTypeTenant }

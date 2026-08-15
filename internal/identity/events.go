package identity

import (
	"fmt"

	"github.com/jusassessoria/platform/lib/events"
)

// Event contracts for the identity slice. Other slices may import these structs
// as the shape they consume — they are the only identity types allowed to cross
// a slice boundary (slices communicate by event, never by entity/repo).
//
// UserProvisioned is a plain shape not yet published (the producer wiring lands
// with the consumer that needs them). TenantProvisioned/OrgProfileUpdated ARE
// published — ProvisionTenant emits the former, the onboarding profile write the
// latter, both through the outbox.

// TypeTenantProvisioned is the dotted id of the event emitted after a tenant is
// created or synced from Clerk (ProvisionTenant, domain.go). Its aggregate is the
// tenant. billing (fatia 2) consumes it to start the tenant's trial subscription.
const TypeTenantProvisioned = "identity.tenant_provisioned"

// TypeOrgProfileUpdated is the dotted id of the event emitted when an escritório's
// company profile is saved during onboarding. Its aggregate is the tenant.
const TypeOrgProfileUpdated = "identity.org_profile_updated"

// TypeMemberJoined is the dotted id of the event emitted when a user joins a
// tenant (an ACTIVE membership is first created or reactivated). Its aggregate is
// the tenant.
const TypeMemberJoined = "identity.member_joined"

// TypeMemberRemoved is the dotted id of the event emitted when a user's membership
// is soft-removed from a tenant (ACTIVE → REMOVED). Its aggregate is the tenant.
const TypeMemberRemoved = "identity.member_removed"

// TypeNotificationRequested is the dotted id of the GENERIC request-to-notify event
// identity EMITS so the notifications slice delivers an aviso (docs §2.5: slices talk
// by event, never by import). It is the one event whose type deliberately lives on
// both sides of the boundary: the notifications slice owns a decode-only struct of the
// same name, and each producer owns the struct that PUBLISHES it — so identity turns a
// member join into an e-mail without importing the notifications package (which would
// drag its Resend/template machinery into every identity binary).
const TypeNotificationRequested = "notification.requested"

// notifyTypeMemberJoined is the template selector carried in the notification's
// payload Type: it names the notifications-side e-mail template rendered for a join.
const notifyTypeMemberJoined = "member_joined"

const aggregateTypeTenant = "tenant"

// TenantProvisioned is emitted, in the SAME transaction as UpsertTenant, after a
// tenant is created or synced from Clerk (ProvisionTenant). UpsertTenant is
// idempotent (ON CONFLICT (clerk_org_id) DO UPDATE), so an at-least-once webhook
// replay calls ProvisionTenant again — rather than trying to detect insert-vs-
// update here, this always publishes, and the EventID is a STABLE key derived from
// TenantID alone (not a fresh uuid per publish): every replay for the same tenant
// mints the identical event id, so a consumer's SeenOrMark dedup (the same
// at-least-once pattern billing already uses for Stripe webhooks) collapses the
// replays into a single effect, exactly like the deadline slice's scheduled marks.
type TenantProvisioned struct {
	events.Base
	TenantID   string `json:"tenant_id"`
	ClerkOrgID string `json:"clerk_org_id"`
}

var _ events.Event = TenantProvisioned{}

func (TenantProvisioned) Type() string          { return TypeTenantProvisioned }
func (TenantProvisioned) AggregateType() string { return aggregateTypeTenant }

// newTenantProvisioned builds the event for a (re)provisioned tenant. The event id
// is the stable "tenant-provisioned:{tenant_id}" key (see TenantProvisioned's
// doc) — deliberately NOT a fresh uuid, so every replay of the same tenant's
// provisioning dedups to one effect downstream.
func newTenantProvisioned(tenant *Tenant) TenantProvisioned {
	return TenantProvisioned{
		Base:       events.Base{EventID: fmt.Sprintf("tenant-provisioned:%s", tenant.ID), Aggregate: tenant.ID},
		TenantID:   tenant.ID,
		ClerkOrgID: tenant.ClerkOrgID,
	}
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

// MemberRemoved is emitted, in the same transaction as the soft-delete, when a
// user's membership is removed from a tenant (ACTIVE → REMOVED). The payload carries
// the internal ids so a consumer can react (revoke access, reassign work) without
// re-reading the membership. Base adds the event id (consumer dedup) and the
// aggregate id (the tenant).
type MemberRemoved struct {
	events.Base
	TenantID  string `json:"tenant_id"`
	AppUserID string `json:"app_user_id"`
}

var _ events.Event = MemberRemoved{}

func (MemberRemoved) Type() string          { return TypeMemberRemoved }
func (MemberRemoved) AggregateType() string { return aggregateTypeTenant }

// NotificationRequested is the producer-side shape identity PUBLISHES to ask the
// notifications slice to deliver an aviso. Its JSON is byte-for-byte the contract the
// notifications slice DECODES (same field tags), so the two never import each other —
// identity turns a member join into an e-mail through the outbox alone (docs §2.5).
//
// NotifyType is the template selector carried as DATA (json "type"), distinct from
// Type() — the events.Event method that names the routed event id. One generic event
// (notification.requested) drives every kind of aviso; NotifyType picks the template.
type NotificationRequested struct {
	events.Base
	TenantID        string         `json:"tenant_id"`
	RecipientUserID string         `json:"recipient_user_id"`
	NotifyType      string         `json:"type"`
	Payload         map[string]any `json:"payload"`
}

var _ events.Event = NotificationRequested{}

func (NotificationRequested) Type() string          { return TypeNotificationRequested }
func (NotificationRequested) AggregateType() string { return aggregateTypeTenant }

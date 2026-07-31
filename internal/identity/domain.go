package identity

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// publisher is the narrow outbox port the use case needs — the producer half of
// the transactional outbox. *events.Outbox satisfies it structurally.
type publisher interface {
	Publish(ctx context.Context, tx database.Tx, ev events.Event) error
}

// UseCase carries the identity use cases: provisioning tenants/users from Clerk
// webhooks (idempotent, at-least-once), resolving a Principal for the auth
// middleware, and the onboarding profile reads/writes. It depends on the
// Repository interface, the outbox publisher and the UnitOfWork, never on the
// concrete pg implementation (docs §2.5).
type UseCase struct {
	repo   Repository
	outbox publisher
	uow    database.UnitOfWork
}

// NewUseCase wires the use cases to their repository, outbox publisher and unit
// of work.
func NewUseCase(repo Repository, outbox publisher, uow database.UnitOfWork) *UseCase {
	return &UseCase{repo: repo, outbox: outbox, uow: uow}
}

// ProvisionTenant creates or refreshes the tenant mirroring a Clerk Organization.
// Idempotent: replaying the same webhook upserts the same row. The tenant table
// has no tenant_id of its own (it IS the tenant), so no RLS scope is applied.
func (uc *UseCase) ProvisionTenant(ctx context.Context, clerkOrgID, name string) (*Tenant, error) {
	var tenant *Tenant
	err := uc.uow.Do(ctx, "", func(tx database.Tx) error {
		var err error
		tenant, err = uc.repo.UpsertTenant(ctx, tx, clerkOrgID, name)
		return err
	})
	if err != nil {
		return nil, err
	}
	return tenant, nil
}

// resolveOrProvisionTenant returns the tenant behind clerkOrgID, provisioning it
// from the org id+name the membership event carries when it does not exist yet.
// This is the order-tolerance the onboarding depends on: an
// organizationMembership.created that races ahead of organization.created (Clerk
// delivers them unordered/concurrently) provisions the tenant on its own instead of
// failing for Svix to retry — so the membership event alone yields tenant+user+
// membership. It reuses ProvisionTenant (the very path organization.created takes),
// whose UpsertTenant is INSERT ... ON CONFLICT (clerk_org_id) DO UPDATE: a create
// racing a concurrent create (or a late organization.created) resolves to the
// existing row rather than a duplicate or a fatal unique violation. A non-not-found
// read error propagates unchanged (fail fast, no blind provisioning).
func (uc *UseCase) resolveOrProvisionTenant(ctx context.Context, clerkOrgID, orgName string) (*Tenant, error) {
	tenant, err := uc.repo.FindTenantByClerkOrg(ctx, clerkOrgID)
	if err == nil {
		return tenant, nil
	}
	if !errors.Is(err, ErrTenantNotFound) {
		return nil, err
	}
	return uc.ProvisionTenant(ctx, clerkOrgID, orgName)
}

// SyncUser resyncs an existing app_user's email, name and phone from a Clerk
// user.updated webhook. Role and tenant are immutable here — membership decides
// those, not a profile edit — so it reuses the stored values and lets UpsertUser's
// ON CONFLICT clause touch only email/name/phone. Idempotent for at-least-once
// delivery. ErrUserNotFound propagates when the update races ahead of the
// membership webhook that first creates the row.
func (uc *UseCase) SyncUser(ctx context.Context, clerkUserID, email, name, phone string) (*AppUser, error) {
	existing, err := uc.repo.FindUserByClerkUser(ctx, clerkUserID)
	if err != nil {
		return nil, err
	}

	var user *AppUser
	err = uc.uow.Do(ctx, existing.TenantID, func(tx database.Tx) error {
		var err error
		user, err = uc.repo.UpsertUser(ctx, tx, clerkUserID, existing.TenantID, email, name, phone, existing.Role)
		return err
	})
	if err != nil {
		return nil, err
	}
	return user, nil
}

// OnMembershipCreated provisions a user's membership in a tenant from an
// organizationMembership.created webhook: it upserts the app_user and its ACTIVE
// membership, and emits identity.member_joined in the SAME transaction the first
// time the user genuinely joins (a new or reactivated membership) — never on an
// at-least-once replay of an already-active one.
//
// Order-tolerant: when the tenant does not exist yet — the membership event raced
// ahead of organization.created — it is provisioned here and now from the org
// id+name the event carries (resolveOrProvisionTenant), so the membership alone
// yields tenant+user+membership and a late organization.created is then an
// idempotent no-op. Multi-org is refused up front (1 user = 1 escritório in v0): a
// Clerk user already living under another tenant yields ErrMembershipConflict rather
// than a blind upsert that would strand a membership under the wrong tenant.
func (uc *UseCase) OnMembershipCreated(ctx context.Context, clerkUserID, clerkOrgID, orgName, clerkMembershipID, email, name string, role Role) (*AppUser, error) {
	if !role.Valid() {
		return nil, ErrInvalidRole
	}

	tenant, err := uc.resolveOrProvisionTenant(ctx, clerkOrgID, orgName)
	if err != nil {
		return nil, err
	}

	// Multi-org guard: refuse a user who already belongs to a different tenant.
	// A first-time user (ErrUserNotFound) is fine — that is the join we provision.
	existing, err := uc.repo.FindUserByClerkUser(ctx, clerkUserID)
	if err != nil && !errors.Is(err, ErrUserNotFound) {
		return nil, err
	}
	if existing != nil && existing.TenantID != tenant.ID {
		return nil, ErrMembershipConflict
	}

	var user *AppUser
	err = uc.uow.Do(ctx, tenant.ID, func(tx database.Tx) error {
		var err error
		// Membership events carry no phone; pass empty so the upsert's COALESCE
		// leaves any phone already synced from user.updated untouched.
		user, err = uc.repo.UpsertUser(ctx, tx, clerkUserID, tenant.ID, email, name, "", role)
		if err != nil {
			return err
		}

		membership, joined, err := uc.repo.UpsertMembership(ctx, tx, tenant.ID, user.ID, clerkMembershipID, role)
		if err != nil {
			return err
		}
		// Replay of an already-active membership: the write is idempotent, but the
		// events must fire once per real join, so skip publishing here.
		if !joined {
			return nil
		}
		// A real join emits two events in this same tx: member_joined (the domain
		// fact other slices react to) and notification.requested (the aviso that
		// turns the join into a welcome e-mail, delivered by the notifications slice
		// without importing it — the generic event carries the template + data).
		if err := uc.outbox.Publish(ctx, tx, newMemberJoined(tenant.ID, membership.AppUserID, membership.Role)); err != nil {
			return err
		}
		return uc.outbox.Publish(ctx, tx, newMemberJoinedNotification(tenant, membership.AppUserID, name))
	})
	if err != nil {
		return nil, err
	}
	return user, nil
}

// Sync provisions tenant+user+membership synchronously (JIT) from a verified Clerk
// token, so the onboarding wizard gets its tenant back in the SAME request instead
// of polling GET /identity/me for the async webhook to land. Clerk Org is still the
// tenant; this only removes the dependency on the webhook's timing.
//
// It is not a second provisioning path: it REUSES OnMembershipCreated — the exact
// find-or-create tenant + upsert user + upsert membership + join-events flow the
// organizationMembership.created webhook takes — passing an EMPTY clerk membership id
// (the JWT carries no membership id). That empty id is written as SQL NULL, and the
// webhook backfills the real id later without this JIT replay ever clearing it (the
// UpsertMembership upsert COALESCEs the incoming NULL against the stored id). Reusing
// that flow means the multi-org guard (ErrMembershipConflict), idempotency (a second
// sync neither duplicates rows nor re-emits member_joined), the invalid-role check
// (ErrInvalidRole) and the member_joined/notification.requested emission all hold for
// free. It then returns the same read model GET /identity/me serves, with tenant_id
// already populated.
func (uc *UseCase) Sync(ctx context.Context, clerkUserID, clerkOrgID, orgName, email, name string, role Role) (Me, error) {
	if _, err := uc.OnMembershipCreated(ctx, clerkUserID, clerkOrgID, orgName, "", email, name, role); err != nil {
		return Me{}, err
	}
	return uc.GetMe(ctx, clerkUserID)
}

// OnMembershipRemoved soft-deletes a user's membership from an
// organizationMembership.deleted webhook: it flips the ACTIVE membership to REMOVED
// (stamping removed_at) and emits identity.member_removed in the SAME transaction —
// but only the first time, when a row genuinely transitions. A replay of an
// already-removed membership (at-least-once delivery) is a no-op that republishes
// nothing, mirroring OnMembershipCreated's join/replay split.
//
// The tenant must already exist (ErrTenantNotFound propagates so Clerk retries a
// deleted event that raced ahead of organization.created). We do NOT reject removing
// the last admin here: the webhook is a fait accompli — Clerk already blocks that in
// its UI — so re-litigating it would only strand our projection out of sync.
func (uc *UseCase) OnMembershipRemoved(ctx context.Context, clerkOrgID, clerkMembershipID string) error {
	tenant, err := uc.repo.FindTenantByClerkOrg(ctx, clerkOrgID)
	if err != nil {
		return err
	}

	return uc.uow.Do(ctx, tenant.ID, func(tx database.Tx) error {
		membership, removed, err := uc.repo.SoftRemoveMembership(ctx, tx, clerkMembershipID)
		if err != nil {
			return err
		}
		// Nothing was ACTIVE (already-removed replay, or unknown clerk id): the write
		// is idempotent, but the event must fire once per real removal, so skip it.
		if !removed {
			return nil
		}
		return uc.outbox.Publish(ctx, tx, newMemberRemoved(membership.TenantID, membership.AppUserID))
	})
}

// OnMembershipUpdated syncs a role change from an organizationMembership.updated
// webhook: it re-points membership.role AND app_user.role in the SAME transaction so
// the link and the authorization role ResolvePrincipal reads never drift. Idempotent
// for at-least-once delivery (re-applying the same role is a no-op); it emits no event
// — a role change is a silent projection update in v0.
//
// The tenant must already exist (ErrTenantNotFound propagates so Clerk retries an
// updated event that raced ahead of organization.created).
func (uc *UseCase) OnMembershipUpdated(ctx context.Context, clerkOrgID, clerkMembershipID string, role Role) error {
	if !role.Valid() {
		return ErrInvalidRole
	}

	tenant, err := uc.repo.FindTenantByClerkOrg(ctx, clerkOrgID)
	if err != nil {
		return err
	}

	return uc.uow.Do(ctx, tenant.ID, func(tx database.Tx) error {
		return uc.repo.UpdateMembershipRole(ctx, tx, clerkMembershipID, role)
	})
}

// ResolvePrincipal looks up the local user + tenant behind a verified Clerk JWT
// and assembles the Principal the auth middleware injects into the request. The
// TenantID is the internal uuid (from app_user), never the Clerk org id.
//
// Authorization gate: the lookup requires an ACTIVE membership (barrier F2-2), so a
// soft-removed member resolves to ErrUserNotFound and the auth boundary answers 401 —
// access is denied the moment the membership goes REMOVED, not on the next login. The
// Role carried is app_user.role, kept in lockstep with the membership by
// OnMembershipUpdated.
func (uc *UseCase) ResolvePrincipal(ctx context.Context, clerkUserID, clerkOrgID string) (Principal, error) {
	tenant, err := uc.repo.FindTenantByClerkOrg(ctx, clerkOrgID)
	if err != nil {
		return Principal{}, err
	}

	user, err := uc.repo.FindActiveUserByClerkUser(ctx, clerkUserID)
	if err != nil {
		return Principal{}, err
	}

	// 1 user = 1 escritório: the user must belong to the org carried by the
	// token. A mismatch is treated as not-found rather than leaking that the
	// user exists under a different tenant.
	if user.TenantID != tenant.ID {
		return Principal{}, ErrUserNotFound
	}

	return Principal{
		UserID:   user.ID,
		TenantID: user.TenantID,
		Role:     user.Role,
	}, nil
}

// GetMe assembles the onboarding read model for a verified Clerk user (the
// AuthUser endpoints run before a tenant exists). UserID always echoes the Clerk
// user id — the stable identity of the caller. A user with no tenant yet is NOT an
// error: the repo's ErrUserNotFound is folded into a Me with nil tenant/gate, so
// the endpoint answers 200 with nulls and the wizard knows onboarding is pending.
func (uc *UseCase) GetMe(ctx context.Context, clerkUserID string) (Me, error) {
	me, err := uc.repo.GetMeByClerkUser(ctx, clerkUserID)
	if errors.Is(err, ErrUserNotFound) {
		return Me{UserID: clerkUserID}, nil
	}
	if err != nil {
		return Me{}, err
	}

	me.UserID = clerkUserID
	return *me, nil
}

// UpdateOrgProfile persists the escritório's company profile and emits
// identity.org_profile_updated in the SAME transaction (transactional outbox): the
// tenant write and the event commit together or not at all. The onboarding gate is
// stamped once and only once (COALESCE in the query), so a replayed PUT is
// idempotent. tenantID comes from the verified principal, never the body.
func (uc *UseCase) UpdateOrgProfile(ctx context.Context, tenantID string, profile OrgProfile) (*Tenant, error) {
	var tenant *Tenant
	err := uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		var err error
		tenant, err = uc.repo.UpdateOrgProfile(ctx, tx, tenantID, profile)
		if err != nil {
			return err
		}
		return uc.outbox.Publish(ctx, tx, newOrgProfileUpdated(tenant))
	})
	if err != nil {
		return nil, err
	}
	return tenant, nil
}

// newOrgProfileUpdated builds the event for a saved profile, minting a fresh v7
// event id (time-ordered) as the aggregate/idempotency key carrier. The aggregate
// is the tenant; the payload carries just enough for a consumer to react without
// re-reading the tenant.
func newOrgProfileUpdated(tenant *Tenant) OrgProfileUpdated {
	return OrgProfileUpdated{
		Base:      events.Base{EventID: uuid.Must(uuid.NewV7()).String(), Aggregate: tenant.ID},
		TenantID:  tenant.ID,
		CNPJ:      tenant.CNPJ,
		TradeName: tenant.TradeName,
	}
}

// newMemberJoined builds the event for a user's join, minting a fresh v7 event id
// (time-ordered) as the aggregate/idempotency key carrier. The aggregate is the
// tenant; the payload carries the internal ids and role so a consumer can react
// without re-reading the membership.
func newMemberJoined(tenantID, appUserID string, role Role) MemberJoined {
	return MemberJoined{
		Base:      events.Base{EventID: uuid.Must(uuid.NewV7()).String(), Aggregate: tenantID},
		TenantID:  tenantID,
		AppUserID: appUserID,
		Role:      role,
	}
}

// newMemberJoinedNotification builds the notification.requested that turns a join
// into a welcome e-mail, minting a fresh v7 event id (time-ordered) as the
// aggregate/idempotency key carrier. The aggregate is the tenant; the recipient is
// the just-joined app_user; the payload carries the template data the member_joined
// e-mail renders (the member's name and the escritório's display name).
func newMemberJoinedNotification(tenant *Tenant, appUserID, memberName string) NotificationRequested {
	return NotificationRequested{
		Base:            events.Base{EventID: uuid.Must(uuid.NewV7()).String(), Aggregate: tenant.ID},
		TenantID:        tenant.ID,
		RecipientUserID: appUserID,
		NotifyType:      notifyTypeMemberJoined,
		Payload: map[string]any{
			"member_name": memberName,
			"org_name":    orgDisplayName(tenant),
		},
	}
}

// orgDisplayName is the escritório's human name for an aviso: the onboarding trade
// name once set, else the Clerk organization name (always present). Keeps the welcome
// e-mail from rendering an empty {{.org_name}} before the profile is completed.
func orgDisplayName(tenant *Tenant) string {
	if tenant.TradeName != "" {
		return tenant.TradeName
	}
	return tenant.Name
}

// newMemberRemoved builds the event for a soft-removed membership, minting a fresh v7
// event id (time-ordered) as the aggregate/idempotency key carrier. The aggregate is
// the tenant; the payload carries the internal ids so a consumer can revoke access
// without re-reading the membership.
func newMemberRemoved(tenantID, appUserID string) MemberRemoved {
	return MemberRemoved{
		Base:      events.Base{EventID: uuid.Must(uuid.NewV7()).String(), Aggregate: tenantID},
		TenantID:  tenantID,
		AppUserID: appUserID,
	}
}

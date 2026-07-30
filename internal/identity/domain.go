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

// ProvisionUser creates or refreshes the app_user mirroring a Clerk User, linked
// to its tenant. Idempotent for at-least-once webhook delivery. The write runs
// under the tenant's RLS scope (isolation barrier 2, docs §4d.4).
func (uc *UseCase) ProvisionUser(ctx context.Context, clerkUserID, clerkOrgID, email, name string, role Role) (*AppUser, error) {
	if !role.Valid() {
		return nil, ErrInvalidRole
	}

	// The tenant must already exist (its webhook fired first). ErrTenantNotFound
	// propagates when a membership event races ahead of organization.created.
	tenant, err := uc.repo.FindTenantByClerkOrg(ctx, clerkOrgID)
	if err != nil {
		return nil, err
	}

	var user *AppUser
	err = uc.uow.Do(ctx, tenant.ID, func(tx database.Tx) error {
		var err error
		// Membership events carry no phone; pass empty so the COALESCE in the
		// upsert leaves any phone already synced from user.updated untouched.
		user, err = uc.repo.UpsertUser(ctx, tx, clerkUserID, tenant.ID, email, name, "", role)
		return err
	})
	if err != nil {
		return nil, err
	}
	return user, nil
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
// The tenant must already exist (ErrTenantNotFound propagates so Clerk retries a
// membership event that raced ahead of organization.created). Multi-org is refused
// up front (1 user = 1 escritório in v0): a Clerk user already living under another
// tenant yields ErrMembershipConflict rather than a blind upsert that would strand
// a membership under the wrong tenant.
func (uc *UseCase) OnMembershipCreated(ctx context.Context, clerkUserID, clerkOrgID, clerkMembershipID, email, name string, role Role) (*AppUser, error) {
	if !role.Valid() {
		return nil, ErrInvalidRole
	}

	tenant, err := uc.repo.FindTenantByClerkOrg(ctx, clerkOrgID)
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
		// event must fire once per real join, so skip publishing here.
		if !joined {
			return nil
		}
		return uc.outbox.Publish(ctx, tx, newMemberJoined(tenant.ID, membership.AppUserID, membership.Role))
	})
	if err != nil {
		return nil, err
	}
	return user, nil
}

// ResolvePrincipal looks up the local user + tenant behind a verified Clerk JWT
// and assembles the Principal the auth middleware injects into the request. The
// TenantID is the internal uuid (from app_user), never the Clerk org id.
func (uc *UseCase) ResolvePrincipal(ctx context.Context, clerkUserID, clerkOrgID string) (Principal, error) {
	tenant, err := uc.repo.FindTenantByClerkOrg(ctx, clerkOrgID)
	if err != nil {
		return Principal{}, err
	}

	user, err := uc.repo.FindUserByClerkUser(ctx, clerkUserID)
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

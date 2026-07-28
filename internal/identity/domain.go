package identity

import (
	"context"

	"github.com/jusassessoria/platform/lib/database"
)

// UseCase carries the identity use cases: provisioning tenants/users from Clerk
// webhooks (idempotent, at-least-once) and resolving a Principal for the auth
// middleware. It depends on the Repository interface and the UnitOfWork, never
// on the concrete pg implementation (docs §2.5).
type UseCase struct {
	repo Repository
	uow  database.UnitOfWork
}

// NewUseCase wires the use cases to their repository and unit of work.
func NewUseCase(repo Repository, uow database.UnitOfWork) *UseCase {
	return &UseCase{repo: repo, uow: uow}
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
		user, err = uc.repo.UpsertUser(ctx, tx, clerkUserID, tenant.ID, email, name, role)
		return err
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

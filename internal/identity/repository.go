package identity

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jusassessoria/platform/internal/identity/identitydb"
	"github.com/jusassessoria/platform/lib/database"
)

// Repository is the persistence port the use cases depend on (they never see the
// concrete impl). Writes receive the caller's transaction — the use case owns
// the boundary, the repo only participates (docs §4b.1); reads run on the pool.
type Repository interface {
	UpsertTenant(ctx context.Context, tx database.Tx, clerkOrgID, name string) (*Tenant, error)
	FindTenantByClerkOrg(ctx context.Context, clerkOrgID string) (*Tenant, error)
	UpsertUser(ctx context.Context, tx database.Tx, clerkUserID, tenantID, email, name, phone string, role Role) (*AppUser, error)
	FindUserByClerkUser(ctx context.Context, clerkUserID string) (*AppUser, error)
	// FindActiveUserByClerkUser resolves the app_user behind a Clerk user only while
	// it still has an ACTIVE membership — the authorization gate for ResolvePrincipal.
	// A soft-removed member yields ErrUserNotFound, exactly like an unknown user.
	FindActiveUserByClerkUser(ctx context.Context, clerkUserID string) (*AppUser, error)
	// UpsertMembership provisions or reactivates the ACTIVE membership linking an
	// app_user to its tenant, inside the caller's tx. The bool return is `joined`:
	// true when this is a real join (a new row, or a REMOVED one reactivated), false
	// when it merely replays an already-ACTIVE membership — the use case emits
	// member_joined only when true.
	UpsertMembership(ctx context.Context, tx database.Tx, tenantID, appUserID, clerkMembershipID string, role Role) (*Membership, bool, error)
	// SoftRemoveMembership flips an ACTIVE membership to REMOVED (stamping removed_at)
	// by its clerk id, inside the caller's tx. The bool return is `removed`: true when
	// a row actually transitioned ACTIVE→REMOVED, false when nothing was active (a
	// replay of an already-removed membership, or an unknown clerk id) — the use case
	// emits member_removed only when true. Mirrors UpsertMembership's `joined`.
	SoftRemoveMembership(ctx context.Context, tx database.Tx, clerkMembershipID string) (*Membership, bool, error)
	// UpdateMembershipRole re-points a membership's role by its clerk id AND syncs
	// app_user.role to match, in one statement inside the caller's tx, so the link
	// and the authorization role never drift. Idempotent; an unknown clerk id is a
	// no-op.
	UpdateMembershipRole(ctx context.Context, tx database.Tx, clerkMembershipID string, role Role) error
	// GetMeByClerkUser reads the onboarding read model (tenant + gate) for a Clerk
	// user. Returns ErrUserNotFound when the user has no tenant yet — the caller
	// folds that into the "not onboarded" state, it is not a 5xx.
	GetMeByClerkUser(ctx context.Context, clerkUserID string) (*Me, error)
	// UpdateOrgProfile persists the company profile onto the caller's tenant inside
	// the caller's tx and returns the saved tenant. Scoped by tenant id (WHERE id).
	UpdateOrgProfile(ctx context.Context, tx database.Tx, tenantID string, profile OrgProfile) (*Tenant, error)
}

// pgRepository is the sqlc-backed implementation. q is bound to the pool for
// reads; writes rebind the generated queries to the passed transaction.
type pgRepository struct {
	q *identitydb.Queries
}

var _ Repository = (*pgRepository)(nil)

// NewRepository binds the generated queries to pool (used for reads). Inject a
// *pgxpool.Pool in production; both it and a mock satisfy identitydb.DBTX.
func NewRepository(pool identitydb.DBTX) Repository {
	return &pgRepository{q: identitydb.New(pool)}
}

// UpsertTenant provisions or refreshes a tenant inside the caller's tx. RETURNING
// always yields a row, so there is no not-found branch here.
func (r *pgRepository) UpsertTenant(ctx context.Context, tx database.Tx, clerkOrgID, name string) (*Tenant, error) {
	row, err := identitydb.New(tx).UpsertTenant(ctx, identitydb.UpsertTenantParams{
		ClerkOrgID: clerkOrgID,
		Name:       name,
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return tenantToEntity(row)
}

func (r *pgRepository) FindTenantByClerkOrg(ctx context.Context, clerkOrgID string) (*Tenant, error) {
	row, err := r.q.GetTenantByClerkOrg(ctx, clerkOrgID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrTenantNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return tenantToEntity(row)
}

// UpsertUser provisions or refreshes an app_user inside the caller's tx. tenantID
// is the internal uuid (a string on the entity), parsed back to uuid.UUID here.
func (r *pgRepository) UpsertUser(ctx context.Context, tx database.Tx, clerkUserID, tenantID, email, name, phone string, role Role) (*AppUser, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	row, err := identitydb.New(tx).UpsertUser(ctx, identitydb.UpsertUserParams{
		ClerkUserID: clerkUserID,
		TenantID:    tid,
		Email:       email,
		Name:        textToNull(name),
		Phone:       textToNull(phone),
		Role:        string(role),
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return userToEntity(row), nil
}

// UpsertMembership provisions or reactivates the membership inside the caller's tx.
// tenantID/appUserID are internal uuids (strings on the entity), parsed back here;
// clerkMembershipID is written as SQL NULL when empty. RETURNING always yields a
// row, so there is no not-found branch — only the `joined` flag distinguishing a
// real join from an at-least-once replay.
func (r *pgRepository) UpsertMembership(ctx context.Context, tx database.Tx, tenantID, appUserID, clerkMembershipID string, role Role) (*Membership, bool, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, false, database.WrapInfra(err)
	}
	uid, err := uuid.Parse(appUserID)
	if err != nil {
		return nil, false, database.WrapInfra(err)
	}

	row, err := identitydb.New(tx).UpsertMembership(ctx, identitydb.UpsertMembershipParams{
		TenantID:          tid,
		AppUserID:         uid,
		ClerkMembershipID: textToNull(clerkMembershipID),
		Role:              string(role),
	})
	if err != nil {
		return nil, false, database.WrapInfra(err)
	}
	return membershipToEntity(row), row.Joined, nil
}

// SoftRemoveMembership flips an ACTIVE membership to REMOVED by its clerk id inside
// the caller's tx. clerkMembershipID is always the non-empty id Clerk carries on the
// deleted webhook, passed by pointer for the nullable column. A no-row result (the
// membership was already REMOVED, or the clerk id is unknown) is NOT an error: it is
// the `removed=false` no-op the use case swallows without republishing.
func (r *pgRepository) SoftRemoveMembership(ctx context.Context, tx database.Tx, clerkMembershipID string) (*Membership, bool, error) {
	row, err := identitydb.New(tx).SoftRemoveMembership(ctx, &clerkMembershipID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, database.WrapInfra(err)
	}
	return membershipModelToEntity(row), true, nil
}

// UpdateMembershipRole re-points the membership role and syncs app_user.role in one
// statement inside the caller's tx. clerkMembershipID is passed by pointer for the
// nullable column; an unknown id updates nothing (idempotent no-op).
func (r *pgRepository) UpdateMembershipRole(ctx context.Context, tx database.Tx, clerkMembershipID string, role Role) error {
	if err := identitydb.New(tx).UpdateMembershipRole(ctx, identitydb.UpdateMembershipRoleParams{
		ClerkMembershipID: &clerkMembershipID,
		Role:              string(role),
	}); err != nil {
		return database.WrapInfra(err)
	}
	return nil
}

func (r *pgRepository) FindUserByClerkUser(ctx context.Context, clerkUserID string) (*AppUser, error) {
	row, err := r.q.GetUserByClerkUser(ctx, clerkUserID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return userToEntity(row), nil
}

// FindActiveUserByClerkUser reads on the pool (a resolution read, no tx). The join
// to an ACTIVE membership is the authorization gate: no active membership → no row →
// the typed ErrUserNotFound the auth boundary maps to 401, never (nil, nil).
func (r *pgRepository) FindActiveUserByClerkUser(ctx context.Context, clerkUserID string) (*AppUser, error) {
	row, err := r.q.GetActiveUserByClerkUser(ctx, clerkUserID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return userToEntity(row), nil
}

// GetMeByClerkUser reads on the pool (a screen read, no tx). A missing row is the
// typed ErrUserNotFound — the caller distinguishes "no tenant yet" from an infra
// fault by that sentinel, never by (nil, nil).
func (r *pgRepository) GetMeByClerkUser(ctx context.Context, clerkUserID string) (*Me, error) {
	row, err := r.q.GetMeByClerkUser(ctx, clerkUserID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return meToEntity(row), nil
}

// UpdateOrgProfile writes the profile onto the tenant inside the caller's tx and
// returns the saved tenant (RETURNING). tenantID is the internal uuid (a string on
// the entity), parsed back to uuid.UUID here; the address is encoded to jsonb at
// this boundary. A no-row result (the id does not exist) is ErrTenantNotFound.
func (r *pgRepository) UpdateOrgProfile(ctx context.Context, tx database.Tx, tenantID string, profile OrgProfile) (*Tenant, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	address, err := encodeAddress(profile.Address)
	if err != nil {
		return nil, database.WrapInfra(err)
	}

	row, err := identitydb.New(tx).UpdateOrgProfile(ctx, identitydb.UpdateOrgProfileParams{
		ID:        tid,
		Cnpj:      &profile.CNPJ,
		LegalName: &profile.LegalName,
		TradeName: &profile.TradeName,
		Address:   address,
		Phone:     textToNull(profile.Phone),
		Email:     textToNull(profile.Email),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrTenantNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return tenantToEntity(row)
}

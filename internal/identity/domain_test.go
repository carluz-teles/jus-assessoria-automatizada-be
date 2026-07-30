package identity

import (
	"context"
	"errors"
	"testing"

	"github.com/jusassessoria/platform/lib/database"
)

// mockRepo is a hand-written Repository double: each method delegates to a func
// field, so every test injects exactly the behavior it needs. Unset fields fail
// loudly (nil call) if a test reaches a path it did not expect.
type mockRepo struct {
	upsertTenant func(ctx context.Context, tx database.Tx, clerkOrgID, name string) (*Tenant, error)
	findTenant   func(ctx context.Context, clerkOrgID string) (*Tenant, error)
	upsertUser   func(ctx context.Context, tx database.Tx, clerkUserID, tenantID, email, name, phone string, role Role) (*AppUser, error)
	findUser     func(ctx context.Context, clerkUserID string) (*AppUser, error)
}

func (m *mockRepo) UpsertTenant(ctx context.Context, tx database.Tx, clerkOrgID, name string) (*Tenant, error) {
	return m.upsertTenant(ctx, tx, clerkOrgID, name)
}

func (m *mockRepo) FindTenantByClerkOrg(ctx context.Context, clerkOrgID string) (*Tenant, error) {
	return m.findTenant(ctx, clerkOrgID)
}

func (m *mockRepo) UpsertUser(ctx context.Context, tx database.Tx, clerkUserID, tenantID, email, name, phone string, role Role) (*AppUser, error) {
	return m.upsertUser(ctx, tx, clerkUserID, tenantID, email, name, phone, role)
}

func (m *mockRepo) FindUserByClerkUser(ctx context.Context, clerkUserID string) (*AppUser, error) {
	return m.findUser(ctx, clerkUserID)
}

// fakeUOW is a no-op unit of work: it records the RLS scope the use case asked
// for and runs fn with a nil tx (the mocked repo never touches it). err injects
// a boundary failure (Begin/Commit) to prove it propagates unwrapped.
type fakeUOW struct {
	scope  string
	called bool
	err    error
}

func (u *fakeUOW) Do(ctx context.Context, tenantID string, fn func(tx database.Tx) error) error {
	u.called = true
	u.scope = tenantID
	if u.err != nil {
		return u.err
	}
	return fn(nil)
}

func TestUseCase_ProvisionTenant(t *testing.T) {
	ctx := context.Background()
	want := &Tenant{ID: "t-1", ClerkOrgID: "org_abc", Name: "Escritório"}

	t.Run("upserts and returns the tenant with no RLS scope", func(t *testing.T) {
		var calls int
		repo := &mockRepo{
			upsertTenant: func(_ context.Context, _ database.Tx, clerkOrgID, name string) (*Tenant, error) {
				calls++
				if clerkOrgID != "org_abc" || name != "Escritório" {
					t.Fatalf("upsert args = (%q, %q)", clerkOrgID, name)
				}
				return want, nil
			},
		}
		uow := &fakeUOW{}

		got, err := NewUseCase(repo, uow).ProvisionTenant(ctx, "org_abc", "Escritório")
		if err != nil {
			t.Fatalf("ProvisionTenant() error = %v", err)
		}
		if got != want {
			t.Fatalf("ProvisionTenant() = %+v, want %+v", got, want)
		}
		// tenant has no tenant_id of its own — the scope must stay empty.
		if uow.scope != "" {
			t.Fatalf("RLS scope = %q, want empty", uow.scope)
		}
		if calls != 1 {
			t.Fatalf("UpsertTenant calls = %d, want 1", calls)
		}
	})

	t.Run("idempotent: replaying the webhook upserts the same tenant", func(t *testing.T) {
		repo := &mockRepo{
			upsertTenant: func(_ context.Context, _ database.Tx, _, _ string) (*Tenant, error) {
				return want, nil // ON CONFLICT ... DO UPDATE always yields the row
			},
		}
		uc := NewUseCase(repo, &fakeUOW{})

		first, err1 := uc.ProvisionTenant(ctx, "org_abc", "Escritório")
		second, err2 := uc.ProvisionTenant(ctx, "org_abc", "Escritório")
		if err1 != nil || err2 != nil {
			t.Fatalf("errors = %v, %v", err1, err2)
		}
		if first != second {
			t.Fatalf("replays diverged: %+v vs %+v", first, second)
		}
	})

	t.Run("propagates a commit failure unwrapped", func(t *testing.T) {
		boom := errors.New("commit failed")
		repo := &mockRepo{
			upsertTenant: func(_ context.Context, _ database.Tx, _, _ string) (*Tenant, error) {
				return want, nil
			},
		}

		got, err := NewUseCase(repo, &fakeUOW{err: boom}).ProvisionTenant(ctx, "org_abc", "x")
		if !errors.Is(err, boom) {
			t.Fatalf("error = %v, want %v", err, boom)
		}
		if got != nil {
			t.Fatalf("tenant = %+v, want nil on error", got)
		}
	})
}

func TestUseCase_ProvisionUser(t *testing.T) {
	ctx := context.Background()
	tenant := &Tenant{ID: "tenant-uuid", ClerkOrgID: "org_abc"}
	want := &AppUser{ID: "u-1", ClerkUserID: "user_xyz", TenantID: "tenant-uuid", Role: RoleLawyer}

	t.Run("invalid role short-circuits before any repo call", func(t *testing.T) {
		repo := &mockRepo{
			findTenant: func(context.Context, string) (*Tenant, error) {
				t.Fatal("FindTenantByClerkOrg must not be called on invalid role")
				return nil, nil
			},
		}
		uow := &fakeUOW{}

		_, err := NewUseCase(repo, uow).ProvisionUser(ctx, "user_xyz", "org_abc", "a@b.com", "Ana", "OWNER")
		if !errors.Is(err, ErrInvalidRole) {
			t.Fatalf("error = %v, want ErrInvalidRole", err)
		}
		if uow.called {
			t.Fatal("unit of work opened despite invalid role")
		}
	})

	t.Run("tenant not provisioned propagates ErrTenantNotFound", func(t *testing.T) {
		repo := &mockRepo{
			findTenant: func(context.Context, string) (*Tenant, error) {
				return nil, ErrTenantNotFound
			},
		}
		uow := &fakeUOW{}

		_, err := NewUseCase(repo, uow).ProvisionUser(ctx, "user_xyz", "org_abc", "a@b.com", "Ana", RoleLawyer)
		if !errors.Is(err, ErrTenantNotFound) {
			t.Fatalf("error = %v, want ErrTenantNotFound", err)
		}
		if uow.called {
			t.Fatal("unit of work opened despite missing tenant")
		}
	})

	t.Run("upserts the user under the tenant's RLS scope", func(t *testing.T) {
		var upsertArgs struct {
			clerkUserID, tenantID, email, name, phone string
			role                                      Role
		}
		repo := &mockRepo{
			findTenant: func(_ context.Context, clerkOrgID string) (*Tenant, error) {
				if clerkOrgID != "org_abc" {
					t.Fatalf("findTenant org = %q", clerkOrgID)
				}
				return tenant, nil
			},
			upsertUser: func(_ context.Context, _ database.Tx, clerkUserID, tenantID, email, name, phone string, role Role) (*AppUser, error) {
				upsertArgs.clerkUserID = clerkUserID
				upsertArgs.tenantID = tenantID
				upsertArgs.email = email
				upsertArgs.name = name
				upsertArgs.phone = phone
				upsertArgs.role = role
				return want, nil
			},
		}
		uow := &fakeUOW{}

		got, err := NewUseCase(repo, uow).ProvisionUser(ctx, "user_xyz", "org_abc", "a@b.com", "Ana", RoleLawyer)
		if err != nil {
			t.Fatalf("ProvisionUser() error = %v", err)
		}
		if got != want {
			t.Fatalf("ProvisionUser() = %+v, want %+v", got, want)
		}
		if uow.scope != tenant.ID {
			t.Fatalf("RLS scope = %q, want %q", uow.scope, tenant.ID)
		}
		if upsertArgs.tenantID != tenant.ID {
			t.Fatalf("upsert tenantID = %q, want internal %q", upsertArgs.tenantID, tenant.ID)
		}
		if upsertArgs.clerkUserID != "user_xyz" || upsertArgs.email != "a@b.com" || upsertArgs.name != "Ana" || upsertArgs.role != RoleLawyer {
			t.Fatalf("upsert args = %+v", upsertArgs)
		}
		// Membership events carry no phone: the use case must pass empty so the
		// upsert's COALESCE preserves any phone synced from user.updated.
		if upsertArgs.phone != "" {
			t.Fatalf("membership provisioning passed phone %q, want empty", upsertArgs.phone)
		}
	})
}

func TestUseCase_SyncUser(t *testing.T) {
	ctx := context.Background()
	existing := &AppUser{ID: "u-1", ClerkUserID: "user_xyz", TenantID: "tenant-uuid", Role: RoleAdmin}

	t.Run("resyncs email/name/phone under the tenant scope, keeping tenant and role", func(t *testing.T) {
		var upsert struct {
			tenantID, email, name, phone string
			role                         Role
		}
		repo := &mockRepo{
			findUser: func(context.Context, string) (*AppUser, error) { return existing, nil },
			upsertUser: func(_ context.Context, _ database.Tx, _, tenantID, email, name, phone string, role Role) (*AppUser, error) {
				upsert.tenantID, upsert.email, upsert.name, upsert.phone, upsert.role = tenantID, email, name, phone, role
				return existing, nil
			},
		}
		uow := &fakeUOW{}

		got, err := NewUseCase(repo, uow).SyncUser(ctx, "user_xyz", "new@b.com", "Ana Nova", "+5511987654321")
		if err != nil {
			t.Fatalf("SyncUser() error = %v", err)
		}
		if got != existing {
			t.Fatalf("SyncUser() = %+v, want %+v", got, existing)
		}
		if uow.scope != existing.TenantID {
			t.Fatalf("RLS scope = %q, want %q", uow.scope, existing.TenantID)
		}
		if upsert.tenantID != existing.TenantID || upsert.role != existing.Role {
			t.Fatalf("tenant/role not preserved: %+v", upsert)
		}
		if upsert.email != "new@b.com" || upsert.name != "Ana Nova" {
			t.Fatalf("resynced fields = %+v", upsert)
		}
		// AC2: the phone from the webhook is propagated to the upsert.
		if upsert.phone != "+5511987654321" {
			t.Fatalf("phone = %q, want propagated to upsert", upsert.phone)
		}
	})

	t.Run("resyncs without a phone passes empty (upsert COALESCE leaves it unchanged)", func(t *testing.T) {
		var gotPhone string
		called := false
		repo := &mockRepo{
			findUser: func(context.Context, string) (*AppUser, error) { return existing, nil },
			upsertUser: func(_ context.Context, _ database.Tx, _, _, _, _, phone string, _ Role) (*AppUser, error) {
				gotPhone = phone
				called = true
				return existing, nil
			},
		}

		// AC3: a user.updated with no phone must not panic and must forward empty
		// (the repo's COALESCE then keeps the stored phone / leaves it null).
		if _, err := NewUseCase(repo, &fakeUOW{}).SyncUser(ctx, "user_xyz", "new@b.com", "Ana Nova", ""); err != nil {
			t.Fatalf("SyncUser() error = %v", err)
		}
		if !called {
			t.Fatal("UpsertUser was not called")
		}
		if gotPhone != "" {
			t.Fatalf("phone = %q, want empty", gotPhone)
		}
	})

	t.Run("missing user propagates ErrUserNotFound without opening a tx", func(t *testing.T) {
		repo := &mockRepo{
			findUser: func(context.Context, string) (*AppUser, error) { return nil, ErrUserNotFound },
		}
		uow := &fakeUOW{}

		_, err := NewUseCase(repo, uow).SyncUser(ctx, "user_xyz", "x@y.com", "X", "")
		if !errors.Is(err, ErrUserNotFound) {
			t.Fatalf("error = %v, want ErrUserNotFound", err)
		}
		if uow.called {
			t.Fatal("unit of work opened despite missing user")
		}
	})
}

func TestUseCase_ResolvePrincipal(t *testing.T) {
	ctx := context.Background()
	tenant := &Tenant{ID: "tenant-uuid", ClerkOrgID: "org_abc"}
	user := &AppUser{ID: "user-uuid", ClerkUserID: "user_xyz", TenantID: "tenant-uuid", Role: RoleAdmin}

	t.Run("assembles the principal from user + tenant", func(t *testing.T) {
		repo := &mockRepo{
			findTenant: func(context.Context, string) (*Tenant, error) { return tenant, nil },
			findUser:   func(context.Context, string) (*AppUser, error) { return user, nil },
		}

		got, err := NewUseCase(repo, &fakeUOW{}).ResolvePrincipal(ctx, "user_xyz", "org_abc")
		if err != nil {
			t.Fatalf("ResolvePrincipal() error = %v", err)
		}
		want := Principal{UserID: "user-uuid", TenantID: "tenant-uuid", Role: RoleAdmin}
		if got != want {
			t.Fatalf("ResolvePrincipal() = %+v, want %+v", got, want)
		}
	})

	t.Run("missing tenant propagates ErrTenantNotFound", func(t *testing.T) {
		repo := &mockRepo{
			findTenant: func(context.Context, string) (*Tenant, error) { return nil, ErrTenantNotFound },
		}

		_, err := NewUseCase(repo, &fakeUOW{}).ResolvePrincipal(ctx, "user_xyz", "org_abc")
		if !errors.Is(err, ErrTenantNotFound) {
			t.Fatalf("error = %v, want ErrTenantNotFound", err)
		}
	})

	t.Run("missing user propagates ErrUserNotFound", func(t *testing.T) {
		repo := &mockRepo{
			findTenant: func(context.Context, string) (*Tenant, error) { return tenant, nil },
			findUser:   func(context.Context, string) (*AppUser, error) { return nil, ErrUserNotFound },
		}

		_, err := NewUseCase(repo, &fakeUOW{}).ResolvePrincipal(ctx, "user_xyz", "org_abc")
		if !errors.Is(err, ErrUserNotFound) {
			t.Fatalf("error = %v, want ErrUserNotFound", err)
		}
	})

	t.Run("user of another tenant is treated as not found", func(t *testing.T) {
		other := &AppUser{ID: "user-uuid", TenantID: "different-tenant", Role: RoleAdmin}
		repo := &mockRepo{
			findTenant: func(context.Context, string) (*Tenant, error) { return tenant, nil },
			findUser:   func(context.Context, string) (*AppUser, error) { return other, nil },
		}

		_, err := NewUseCase(repo, &fakeUOW{}).ResolvePrincipal(ctx, "user_xyz", "org_abc")
		if !errors.Is(err, ErrUserNotFound) {
			t.Fatalf("error = %v, want ErrUserNotFound on tenant mismatch", err)
		}
	})
}

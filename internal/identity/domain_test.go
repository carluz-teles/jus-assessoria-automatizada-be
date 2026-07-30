package identity

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// mockRepo is a hand-written Repository double: each method delegates to a func
// field, so every test injects exactly the behavior it needs. Unset fields fail
// loudly (nil call) if a test reaches a path it did not expect.
type mockRepo struct {
	upsertTenant     func(ctx context.Context, tx database.Tx, clerkOrgID, name string) (*Tenant, error)
	findTenant       func(ctx context.Context, clerkOrgID string) (*Tenant, error)
	upsertUser       func(ctx context.Context, tx database.Tx, clerkUserID, tenantID, email, name, phone string, role Role) (*AppUser, error)
	findUser         func(ctx context.Context, clerkUserID string) (*AppUser, error)
	getMe            func(ctx context.Context, clerkUserID string) (*Me, error)
	updateOrgProfile func(ctx context.Context, tx database.Tx, tenantID string, profile OrgProfile) (*Tenant, error)
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

func (m *mockRepo) GetMeByClerkUser(ctx context.Context, clerkUserID string) (*Me, error) {
	return m.getMe(ctx, clerkUserID)
}

func (m *mockRepo) UpdateOrgProfile(ctx context.Context, tx database.Tx, tenantID string, profile OrgProfile) (*Tenant, error) {
	return m.updateOrgProfile(ctx, tx, tenantID, profile)
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

// noopOutbox is a publisher that drops every event — used by the provisioning and
// resolution use cases, which emit nothing, so their tests do not care about it.
type noopOutbox struct{}

func (noopOutbox) Publish(context.Context, database.Tx, events.Event) error { return nil }

// recordingOutbox captures what a use case publishes (and can inject a publish
// failure) so the UpdateOrgProfile tests can assert the org_profile_updated event
// is emitted in the same unit of work.
type recordingOutbox struct {
	published []events.Event
	err       error
}

func (r *recordingOutbox) Publish(_ context.Context, _ database.Tx, ev events.Event) error {
	if r.err != nil {
		return r.err
	}
	r.published = append(r.published, ev)
	return nil
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

		got, err := NewUseCase(repo, noopOutbox{}, uow).ProvisionTenant(ctx, "org_abc", "Escritório")
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
		uc := NewUseCase(repo, noopOutbox{}, &fakeUOW{})

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

		got, err := NewUseCase(repo, noopOutbox{}, &fakeUOW{err: boom}).ProvisionTenant(ctx, "org_abc", "x")
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

		_, err := NewUseCase(repo, noopOutbox{}, uow).ProvisionUser(ctx, "user_xyz", "org_abc", "a@b.com", "Ana", "OWNER")
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

		_, err := NewUseCase(repo, noopOutbox{}, uow).ProvisionUser(ctx, "user_xyz", "org_abc", "a@b.com", "Ana", RoleLawyer)
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

		got, err := NewUseCase(repo, noopOutbox{}, uow).ProvisionUser(ctx, "user_xyz", "org_abc", "a@b.com", "Ana", RoleLawyer)
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

		got, err := NewUseCase(repo, noopOutbox{}, uow).SyncUser(ctx, "user_xyz", "new@b.com", "Ana Nova", "+5511987654321")
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
		if _, err := NewUseCase(repo, noopOutbox{}, &fakeUOW{}).SyncUser(ctx, "user_xyz", "new@b.com", "Ana Nova", ""); err != nil {
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

		_, err := NewUseCase(repo, noopOutbox{}, uow).SyncUser(ctx, "user_xyz", "x@y.com", "X", "")
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

		got, err := NewUseCase(repo, noopOutbox{}, &fakeUOW{}).ResolvePrincipal(ctx, "user_xyz", "org_abc")
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

		_, err := NewUseCase(repo, noopOutbox{}, &fakeUOW{}).ResolvePrincipal(ctx, "user_xyz", "org_abc")
		if !errors.Is(err, ErrTenantNotFound) {
			t.Fatalf("error = %v, want ErrTenantNotFound", err)
		}
	})

	t.Run("missing user propagates ErrUserNotFound", func(t *testing.T) {
		repo := &mockRepo{
			findTenant: func(context.Context, string) (*Tenant, error) { return tenant, nil },
			findUser:   func(context.Context, string) (*AppUser, error) { return nil, ErrUserNotFound },
		}

		_, err := NewUseCase(repo, noopOutbox{}, &fakeUOW{}).ResolvePrincipal(ctx, "user_xyz", "org_abc")
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

		_, err := NewUseCase(repo, noopOutbox{}, &fakeUOW{}).ResolvePrincipal(ctx, "user_xyz", "org_abc")
		if !errors.Is(err, ErrUserNotFound) {
			t.Fatalf("error = %v, want ErrUserNotFound on tenant mismatch", err)
		}
	})
}

func TestUseCase_GetMe(t *testing.T) {
	ctx := context.Background()
	const clerkUser = "user_xyz"

	t.Run("user with tenant returns the internal tenant and gate, user_id echoing the clerk id", func(t *testing.T) {
		tenantID := "tenant-uuid"
		onboarded := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
		repo := &mockRepo{
			getMe: func(_ context.Context, gotClerkUser string) (*Me, error) {
				if gotClerkUser != clerkUser {
					t.Fatalf("getMe clerk user = %q, want %q", gotClerkUser, clerkUser)
				}
				return &Me{TenantID: &tenantID, OnboardingCompletedAt: &onboarded}, nil
			},
		}

		got, err := NewUseCase(repo, noopOutbox{}, &fakeUOW{}).GetMe(ctx, clerkUser)
		if err != nil {
			t.Fatalf("GetMe() error = %v", err)
		}
		if got.UserID != clerkUser {
			t.Fatalf("UserID = %q, want the clerk id %q", got.UserID, clerkUser)
		}
		if got.TenantID == nil || *got.TenantID != tenantID {
			t.Fatalf("TenantID = %v, want %q", got.TenantID, tenantID)
		}
		if got.OnboardingCompletedAt == nil || !got.OnboardingCompletedAt.Equal(onboarded) {
			t.Fatalf("OnboardingCompletedAt = %v, want %v", got.OnboardingCompletedAt, onboarded)
		}
	})

	t.Run("user without tenant is a 200 with nulls, not an error", func(t *testing.T) {
		repo := &mockRepo{
			getMe: func(context.Context, string) (*Me, error) { return nil, ErrUserNotFound },
		}

		got, err := NewUseCase(repo, noopOutbox{}, &fakeUOW{}).GetMe(ctx, clerkUser)
		if err != nil {
			t.Fatalf("GetMe() error = %v, want nil (no tenant is not an error)", err)
		}
		if got.UserID != clerkUser {
			t.Fatalf("UserID = %q, want the clerk id %q", got.UserID, clerkUser)
		}
		if got.TenantID != nil || got.OnboardingCompletedAt != nil {
			t.Fatalf("tenant/gate = %+v, want nil for a user with no tenant", got)
		}
	})

	t.Run("an infra error from the repo propagates", func(t *testing.T) {
		boom := errors.New("db down")
		repo := &mockRepo{
			getMe: func(context.Context, string) (*Me, error) { return nil, boom },
		}

		_, err := NewUseCase(repo, noopOutbox{}, &fakeUOW{}).GetMe(ctx, clerkUser)
		if !errors.Is(err, boom) {
			t.Fatalf("error = %v, want %v", err, boom)
		}
	})
}

func TestUseCase_UpdateOrgProfile(t *testing.T) {
	ctx := context.Background()
	const tenantID = "tenant-uuid"
	profile := OrgProfile{
		CNPJ:      "12345678000195",
		LegalName: "Escritório LTDA",
		TradeName: "Escritório",
		Address:   Address{CEP: "01311902", Logradouro: "Av Paulista", Cidade: "São Paulo", UF: "SP"},
	}
	saved := &Tenant{ID: tenantID, CNPJ: profile.CNPJ, LegalName: profile.LegalName, TradeName: profile.TradeName}

	t.Run("persists under the tenant scope and emits org_profile_updated in the same UoW", func(t *testing.T) {
		var gotTenantID string
		var gotProfile OrgProfile
		repo := &mockRepo{
			updateOrgProfile: func(_ context.Context, _ database.Tx, tid string, p OrgProfile) (*Tenant, error) {
				gotTenantID, gotProfile = tid, p
				return saved, nil
			},
		}
		outbox := &recordingOutbox{}
		uow := &fakeUOW{}

		got, err := NewUseCase(repo, outbox, uow).UpdateOrgProfile(ctx, tenantID, profile)
		if err != nil {
			t.Fatalf("UpdateOrgProfile() error = %v", err)
		}
		if got != saved {
			t.Fatalf("UpdateOrgProfile() = %+v, want %+v", got, saved)
		}
		if gotTenantID != tenantID || gotProfile != profile {
			t.Fatalf("repo received (tenant=%q, profile=%+v)", gotTenantID, gotProfile)
		}
		// The write ran under the tenant's RLS scope (barrier 2).
		if uow.scope != tenantID {
			t.Fatalf("RLS scope = %q, want %q", uow.scope, tenantID)
		}
		// Exactly one event, published inside the same unit of work (AC3).
		if len(outbox.published) != 1 {
			t.Fatalf("published %d events, want 1", len(outbox.published))
		}
		ev, ok := outbox.published[0].(OrgProfileUpdated)
		if !ok {
			t.Fatalf("event type = %T, want OrgProfileUpdated", outbox.published[0])
		}
		if ev.Type() != TypeOrgProfileUpdated || ev.AggregateType() != aggregateTypeTenant {
			t.Fatalf("event ids = (%q, %q)", ev.Type(), ev.AggregateType())
		}
		if ev.AggregateID() != tenantID || ev.TenantID != tenantID {
			t.Fatalf("event aggregate/tenant = (%q, %q), want %q", ev.AggregateID(), ev.TenantID, tenantID)
		}
		if ev.CNPJ != profile.CNPJ || ev.TradeName != profile.TradeName {
			t.Fatalf("event payload = %+v", ev)
		}
		if ev.IdempotencyKey() == "" {
			t.Fatal("event idempotency key (event id) is empty")
		}
	})

	t.Run("a publish failure rolls back — the error propagates and nothing is returned", func(t *testing.T) {
		repo := &mockRepo{
			updateOrgProfile: func(context.Context, database.Tx, string, OrgProfile) (*Tenant, error) {
				return saved, nil
			},
		}
		boom := errors.New("outbox unreachable")

		got, err := NewUseCase(repo, &recordingOutbox{err: boom}, &fakeUOW{}).UpdateOrgProfile(ctx, tenantID, profile)
		if !errors.Is(err, boom) {
			t.Fatalf("error = %v, want %v", err, boom)
		}
		if got != nil {
			t.Fatalf("tenant = %+v, want nil on publish failure", got)
		}
	})

	t.Run("a repo error propagates and no event is published", func(t *testing.T) {
		repo := &mockRepo{
			updateOrgProfile: func(context.Context, database.Tx, string, OrgProfile) (*Tenant, error) {
				return nil, ErrTenantNotFound
			},
		}
		outbox := &recordingOutbox{}

		_, err := NewUseCase(repo, outbox, &fakeUOW{}).UpdateOrgProfile(ctx, tenantID, profile)
		if !errors.Is(err, ErrTenantNotFound) {
			t.Fatalf("error = %v, want ErrTenantNotFound", err)
		}
		if len(outbox.published) != 0 {
			t.Fatalf("published %d events, want 0 when the write failed", len(outbox.published))
		}
	})
}

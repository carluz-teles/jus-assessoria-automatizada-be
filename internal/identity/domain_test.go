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
	findTenantByID   func(ctx context.Context, tenantID string) (*Tenant, error)
	upsertUser       func(ctx context.Context, tx database.Tx, clerkUserID, tenantID, email, name, phone string, role Role) (*AppUser, error)
	upsertMembership func(ctx context.Context, tx database.Tx, tenantID, appUserID, clerkMembershipID string, role Role) (*Membership, bool, error)
	softRemoveMember func(ctx context.Context, tx database.Tx, clerkMembershipID string) (*Membership, bool, error)
	updateMemberRole func(ctx context.Context, tx database.Tx, clerkMembershipID string, role Role) error
	findUser         func(ctx context.Context, clerkUserID string) (*AppUser, error)
	findActiveUser   func(ctx context.Context, clerkUserID string) (*AppUser, error)
	getMe            func(ctx context.Context, clerkUserID string) (*Me, error)
	updateOrgProfile func(ctx context.Context, tx database.Tx, tenantID string, profile OrgProfile) (*Tenant, error)
	listOrgMembers   func(ctx context.Context, tenantID string) ([]OrgMember, error)
	findMemberClerk  func(ctx context.Context, tenantID, appUserID string) (string, error)
}

func (m *mockRepo) UpsertTenant(ctx context.Context, tx database.Tx, clerkOrgID, name string) (*Tenant, error) {
	return m.upsertTenant(ctx, tx, clerkOrgID, name)
}

func (m *mockRepo) FindTenantByClerkOrg(ctx context.Context, clerkOrgID string) (*Tenant, error) {
	return m.findTenant(ctx, clerkOrgID)
}

func (m *mockRepo) FindTenantByID(ctx context.Context, tenantID string) (*Tenant, error) {
	return m.findTenantByID(ctx, tenantID)
}

func (m *mockRepo) UpsertUser(ctx context.Context, tx database.Tx, clerkUserID, tenantID, email, name, phone string, role Role) (*AppUser, error) {
	return m.upsertUser(ctx, tx, clerkUserID, tenantID, email, name, phone, role)
}

func (m *mockRepo) UpsertMembership(ctx context.Context, tx database.Tx, tenantID, appUserID, clerkMembershipID string, role Role) (*Membership, bool, error) {
	return m.upsertMembership(ctx, tx, tenantID, appUserID, clerkMembershipID, role)
}

func (m *mockRepo) SoftRemoveMembership(ctx context.Context, tx database.Tx, clerkMembershipID string) (*Membership, bool, error) {
	return m.softRemoveMember(ctx, tx, clerkMembershipID)
}

func (m *mockRepo) UpdateMembershipRole(ctx context.Context, tx database.Tx, clerkMembershipID string, role Role) error {
	return m.updateMemberRole(ctx, tx, clerkMembershipID, role)
}

func (m *mockRepo) FindUserByClerkUser(ctx context.Context, clerkUserID string) (*AppUser, error) {
	return m.findUser(ctx, clerkUserID)
}

func (m *mockRepo) FindActiveUserByClerkUser(ctx context.Context, clerkUserID string) (*AppUser, error) {
	return m.findActiveUser(ctx, clerkUserID)
}

func (m *mockRepo) GetMeByClerkUser(ctx context.Context, clerkUserID string) (*Me, error) {
	return m.getMe(ctx, clerkUserID)
}

func (m *mockRepo) UpdateOrgProfile(ctx context.Context, tx database.Tx, tenantID string, profile OrgProfile) (*Tenant, error) {
	return m.updateOrgProfile(ctx, tx, tenantID, profile)
}

func (m *mockRepo) ListOrgMembers(ctx context.Context, tenantID string) ([]OrgMember, error) {
	return m.listOrgMembers(ctx, tenantID)
}

func (m *mockRepo) FindActiveMemberClerkUser(ctx context.Context, tenantID, appUserID string) (string, error) {
	return m.findMemberClerk(ctx, tenantID, appUserID)
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

func (u *fakeUOW) DoSystem(_ context.Context, fn func(tx database.Tx) error) error {
	u.called = true
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

	t.Run("publishes tenant_provisioned in the same tx, with a stable event id across replays", func(t *testing.T) {
		repo := &mockRepo{
			upsertTenant: func(_ context.Context, _ database.Tx, _, _ string) (*Tenant, error) {
				return want, nil // ON CONFLICT ... DO UPDATE always yields the row
			},
		}
		outbox := &recordingOutbox{}
		uc := NewUseCase(repo, outbox, &fakeUOW{})

		if _, err := uc.ProvisionTenant(ctx, "org_abc", "Escritório"); err != nil {
			t.Fatalf("ProvisionTenant() error = %v", err)
		}
		if _, err := uc.ProvisionTenant(ctx, "org_abc", "Escritório"); err != nil {
			t.Fatalf("ProvisionTenant() (replay) error = %v", err)
		}

		if len(outbox.published) != 2 {
			t.Fatalf("published %d events, want 2 (one tenant_provisioned per call)", len(outbox.published))
		}
		first, ok := outbox.published[0].(TenantProvisioned)
		if !ok || first.TenantID != want.ID || first.ClerkOrgID != want.ClerkOrgID {
			t.Fatalf("event[0] = %+v, want TenantProvisioned for %+v", outbox.published[0], want)
		}
		second, ok := outbox.published[1].(TenantProvisioned)
		if !ok {
			t.Fatalf("event[1] = %+v, want TenantProvisioned", outbox.published[1])
		}
		// A consumer dedups by IdempotencyKey — a replay must mint the SAME key so
		// billing's SeenOrMark collapses it into one trial provisioning, never two.
		if first.IdempotencyKey() != second.IdempotencyKey() {
			t.Fatalf("idempotency key drifted across replays: %q vs %q", first.IdempotencyKey(), second.IdempotencyKey())
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

func TestUseCase_OnMembershipCreated(t *testing.T) {
	ctx := context.Background()
	tenant := &Tenant{ID: "tenant-uuid", ClerkOrgID: "org_abc"}
	user := &AppUser{ID: "user-uuid", ClerkUserID: "user_xyz", TenantID: tenant.ID, Role: RoleLawyer}
	membership := &Membership{ID: "m-1", TenantID: tenant.ID, AppUserID: user.ID, Role: RoleLawyer, Status: MembershipActive}

	t.Run("new join upserts user + membership and emits member_joined in the same UoW", func(t *testing.T) {
		var upsertU struct {
			clerkUserID, tenantID, email, name, phone string
			role                                      Role
		}
		var upsertM struct {
			tenantID, appUserID, clerkMembershipID string
			role                                   Role
		}
		repo := &mockRepo{
			findTenant: func(context.Context, string) (*Tenant, error) { return tenant, nil },
			findUser:   func(context.Context, string) (*AppUser, error) { return nil, ErrUserNotFound },
			upsertUser: func(_ context.Context, _ database.Tx, clerkUserID, tenantID, email, name, phone string, role Role) (*AppUser, error) {
				upsertU.clerkUserID, upsertU.tenantID, upsertU.email, upsertU.name, upsertU.phone, upsertU.role = clerkUserID, tenantID, email, name, phone, role
				return user, nil
			},
			upsertMembership: func(_ context.Context, _ database.Tx, tenantID, appUserID, clerkMembershipID string, role Role) (*Membership, bool, error) {
				upsertM.tenantID, upsertM.appUserID, upsertM.clerkMembershipID, upsertM.role = tenantID, appUserID, clerkMembershipID, role
				return membership, true, nil // joined
			},
		}
		outbox := &recordingOutbox{}
		uow := &fakeUOW{}

		got, err := NewUseCase(repo, outbox, uow).OnMembershipCreated(ctx, "user_xyz", "org_abc", "Escritório", "orgmem_1", "ana@b.com", "Ana", RoleLawyer)
		if err != nil {
			t.Fatalf("OnMembershipCreated() error = %v", err)
		}
		if got != user {
			t.Fatalf("OnMembershipCreated() = %+v, want %+v", got, user)
		}
		// The membership was upserted under the tenant's RLS scope (barrier 2) with
		// the internal ids and the clerk membership id.
		if uow.scope != tenant.ID {
			t.Fatalf("RLS scope = %q, want %q", uow.scope, tenant.ID)
		}
		// The app_user is upserted with the internal tenant id and the membership's
		// role. Membership events carry no phone, so the use case must pass empty and
		// let the upsert's COALESCE preserve any phone synced from user.updated.
		if upsertU.tenantID != tenant.ID {
			t.Fatalf("upsert tenantID = %q, want internal %q", upsertU.tenantID, tenant.ID)
		}
		if upsertU.clerkUserID != "user_xyz" || upsertU.email != "ana@b.com" || upsertU.name != "Ana" || upsertU.role != RoleLawyer {
			t.Fatalf("UpsertUser args = %+v", upsertU)
		}
		if upsertU.phone != "" {
			t.Fatalf("membership provisioning passed phone %q, want empty", upsertU.phone)
		}
		if upsertM.tenantID != tenant.ID || upsertM.appUserID != user.ID || upsertM.clerkMembershipID != "orgmem_1" || upsertM.role != RoleLawyer {
			t.Fatalf("UpsertMembership args = %+v", upsertM)
		}
		// Two events, in order, published inside the same unit of work: the domain
		// fact member_joined, then the notification.requested that turns the join into
		// a welcome e-mail.
		if len(outbox.published) != 2 {
			t.Fatalf("published %d events, want 2 (member_joined + notification.requested)", len(outbox.published))
		}
		ev, ok := outbox.published[0].(MemberJoined)
		if !ok {
			t.Fatalf("event[0] type = %T, want MemberJoined", outbox.published[0])
		}
		if ev.Type() != TypeMemberJoined || ev.AggregateType() != aggregateTypeTenant {
			t.Fatalf("event ids = (%q, %q)", ev.Type(), ev.AggregateType())
		}
		if ev.AggregateID() != tenant.ID || ev.TenantID != tenant.ID || ev.AppUserID != user.ID || ev.Role != RoleLawyer {
			t.Fatalf("event payload = %+v", ev)
		}
		if ev.IdempotencyKey() == "" {
			t.Fatal("event idempotency key (event id) is empty")
		}

		// AC1: the notification.requested carries the routed type, the tenant
		// aggregate, the recipient (the just-joined app_user) and the template data;
		// its own fresh idempotency key means a relay replay dedups it independently.
		notif, ok := outbox.published[1].(NotificationRequested)
		if !ok {
			t.Fatalf("event[1] type = %T, want NotificationRequested", outbox.published[1])
		}
		if notif.Type() != TypeNotificationRequested || notif.AggregateType() != aggregateTypeTenant {
			t.Fatalf("notification ids = (%q, %q)", notif.Type(), notif.AggregateType())
		}
		if notif.AggregateID() != tenant.ID || notif.TenantID != tenant.ID || notif.RecipientUserID != user.ID {
			t.Fatalf("notification envelope = %+v", notif)
		}
		if notif.NotifyType != notifyTypeMemberJoined {
			t.Fatalf("notification template selector = %q, want %q", notif.NotifyType, notifyTypeMemberJoined)
		}
		if notif.Payload["member_name"] != "Ana" {
			t.Fatalf("notification payload = %+v, want member_name Ana", notif.Payload)
		}
		if notif.IdempotencyKey() == "" || notif.IdempotencyKey() == ev.IdempotencyKey() {
			t.Fatalf("notification needs its own non-empty event id, got %q (member_joined %q)", notif.IdempotencyKey(), ev.IdempotencyKey())
		}
	})

	t.Run("replay of an active membership upserts but publishes no event", func(t *testing.T) {
		repo := &mockRepo{
			findTenant: func(context.Context, string) (*Tenant, error) { return tenant, nil },
			findUser:   func(context.Context, string) (*AppUser, error) { return user, nil },
			upsertUser: func(_ context.Context, _ database.Tx, _, _, _, _, _ string, _ Role) (*AppUser, error) {
				return user, nil
			},
			upsertMembership: func(_ context.Context, _ database.Tx, _, _, _ string, _ Role) (*Membership, bool, error) {
				return membership, false, nil // already ACTIVE — not a real join
			},
		}
		outbox := &recordingOutbox{}

		got, err := NewUseCase(repo, outbox, &fakeUOW{}).OnMembershipCreated(ctx, "user_xyz", "org_abc", "Escritório", "orgmem_1", "ana@b.com", "Ana", RoleLawyer)
		if err != nil {
			t.Fatalf("OnMembershipCreated() error = %v", err)
		}
		if got != user {
			t.Fatalf("OnMembershipCreated() = %+v, want %+v", got, user)
		}
		if len(outbox.published) != 0 {
			t.Fatalf("published %d events on replay, want 0", len(outbox.published))
		}
	})

	t.Run("a user already in another tenant is refused with a conflict, no tx", func(t *testing.T) {
		other := &AppUser{ID: "user-uuid", ClerkUserID: "user_xyz", TenantID: "different-tenant", Role: RoleAdmin}
		repo := &mockRepo{
			findTenant: func(context.Context, string) (*Tenant, error) { return tenant, nil },
			findUser:   func(context.Context, string) (*AppUser, error) { return other, nil },
		}
		uow := &fakeUOW{}

		_, err := NewUseCase(repo, noopOutbox{}, uow).OnMembershipCreated(ctx, "user_xyz", "org_abc", "Escritório", "orgmem_1", "ana@b.com", "Ana", RoleLawyer)
		if !errors.Is(err, ErrMembershipConflict) {
			t.Fatalf("error = %v, want ErrMembershipConflict", err)
		}
		if uow.called {
			t.Fatal("unit of work opened despite the multi-org conflict")
		}
	})

	// AC1: an organizationMembership.created that races ahead of organization.created
	// (the tenant does not exist yet) provisions the tenant from the org id+name the
	// event carries, then creates user + membership and emits the join events — no
	// error, no dependence on a Svix retry.
	t.Run("membership racing ahead of organization.created provisions the tenant then user + membership", func(t *testing.T) {
		provisioned := &Tenant{ID: "tenant-uuid", ClerkOrgID: "org_abc", Name: "Escritório"}
		var upsertTenantArgs struct{ clerkOrgID, name string }
		var upsertUTenantID string
		var upsertMCalled bool
		repo := &mockRepo{
			// Tenant not there yet — the membership event won the race.
			findTenant: func(context.Context, string) (*Tenant, error) { return nil, ErrTenantNotFound },
			// ProvisionTenant's UpsertTenant provisions it now, from the event's org data.
			upsertTenant: func(_ context.Context, _ database.Tx, clerkOrgID, name string) (*Tenant, error) {
				upsertTenantArgs.clerkOrgID, upsertTenantArgs.name = clerkOrgID, name
				return provisioned, nil
			},
			findUser: func(context.Context, string) (*AppUser, error) { return nil, ErrUserNotFound },
			upsertUser: func(_ context.Context, _ database.Tx, _, tenantID, _, _, _ string, _ Role) (*AppUser, error) {
				upsertUTenantID = tenantID
				return user, nil
			},
			upsertMembership: func(_ context.Context, _ database.Tx, _, _, _ string, _ Role) (*Membership, bool, error) {
				upsertMCalled = true
				return membership, true, nil // a real join
			},
		}
		outbox := &recordingOutbox{}

		got, err := NewUseCase(repo, outbox, &fakeUOW{}).OnMembershipCreated(ctx, "user_xyz", "org_abc", "Escritório", "orgmem_1", "ana@b.com", "Ana", RoleLawyer)
		if err != nil {
			t.Fatalf("OnMembershipCreated() error = %v", err)
		}
		if got != user {
			t.Fatalf("OnMembershipCreated() = %+v, want %+v", got, user)
		}
		// The tenant was provisioned from the org id+name carried by the membership event.
		if upsertTenantArgs.clerkOrgID != "org_abc" || upsertTenantArgs.name != "Escritório" {
			t.Fatalf("UpsertTenant args = %+v, want the org id+name from the membership event", upsertTenantArgs)
		}
		// user + membership were then created under the freshly provisioned tenant.
		if upsertUTenantID != provisioned.ID {
			t.Fatalf("UpsertUser tenantID = %q, want the provisioned tenant %q", upsertUTenantID, provisioned.ID)
		}
		if !upsertMCalled {
			t.Fatal("membership was not upserted after provisioning the tenant")
		}
		// AC5: provisioning the tenant emits tenant_provisioned (fatia 2, ProvisionTenant),
		// then the join still emits its two events exactly once (member_joined + notification).
		if len(outbox.published) != 3 {
			t.Fatalf("published %d events, want 3 (tenant_provisioned + member_joined + notification.requested)", len(outbox.published))
		}
		if ev, ok := outbox.published[0].(TenantProvisioned); !ok || ev.TenantID != provisioned.ID {
			t.Fatalf("event[0] = %+v, want TenantProvisioned for %q", outbox.published[0], provisioned.ID)
		}
	})

	// A read error that is NOT ErrTenantNotFound (an infra fault) must propagate
	// unchanged — the use case provisions only on a genuine "not there yet", never
	// blindly on any failure.
	t.Run("a non-not-found tenant read error propagates without provisioning", func(t *testing.T) {
		boom := errors.New("db down")
		repo := &mockRepo{
			findTenant: func(context.Context, string) (*Tenant, error) { return nil, boom },
			upsertTenant: func(context.Context, database.Tx, string, string) (*Tenant, error) {
				t.Fatal("UpsertTenant must not run when the tenant read fails with a non-not-found error")
				return nil, nil
			},
		}
		uow := &fakeUOW{}

		_, err := NewUseCase(repo, noopOutbox{}, uow).OnMembershipCreated(ctx, "user_xyz", "org_abc", "Escritório", "orgmem_1", "ana@b.com", "Ana", RoleLawyer)
		if !errors.Is(err, boom) {
			t.Fatalf("error = %v, want %v", err, boom)
		}
		if uow.called {
			t.Fatal("unit of work opened despite the tenant read failure")
		}
	})

	t.Run("invalid role short-circuits before any repo call", func(t *testing.T) {
		repo := &mockRepo{
			findTenant: func(context.Context, string) (*Tenant, error) {
				t.Fatal("FindTenantByClerkOrg must not be called on invalid role")
				return nil, nil
			},
		}
		uow := &fakeUOW{}

		_, err := NewUseCase(repo, noopOutbox{}, uow).OnMembershipCreated(ctx, "user_xyz", "org_abc", "Escritório", "orgmem_1", "a@b.com", "Ana", "MEMBER")
		if !errors.Is(err, ErrInvalidRole) {
			t.Fatalf("error = %v, want ErrInvalidRole", err)
		}
		if uow.called {
			t.Fatal("unit of work opened despite invalid role")
		}
	})
}

func TestUseCase_OnMembershipRemoved(t *testing.T) {
	ctx := context.Background()
	tenant := &Tenant{ID: "tenant-uuid", ClerkOrgID: "org_abc"}
	membership := &Membership{ID: "m-1", TenantID: tenant.ID, AppUserID: "user-uuid", Status: MembershipRemoved}

	t.Run("active membership is soft-removed and emits member_removed in the same UoW", func(t *testing.T) {
		var gotClerkMemID string
		repo := &mockRepo{
			findTenant: func(context.Context, string) (*Tenant, error) { return tenant, nil },
			softRemoveMember: func(_ context.Context, _ database.Tx, clerkMembershipID string) (*Membership, bool, error) {
				gotClerkMemID = clerkMembershipID
				return membership, true, nil // ACTIVE → REMOVED
			},
		}
		outbox := &recordingOutbox{}
		uow := &fakeUOW{}

		if err := NewUseCase(repo, outbox, uow).OnMembershipRemoved(ctx, "org_abc", "orgmem_1"); err != nil {
			t.Fatalf("OnMembershipRemoved() error = %v", err)
		}
		// The soft-delete ran under the tenant's RLS scope (barrier 2), keyed by the
		// clerk membership id.
		if uow.scope != tenant.ID {
			t.Fatalf("RLS scope = %q, want %q", uow.scope, tenant.ID)
		}
		if gotClerkMemID != "orgmem_1" {
			t.Fatalf("SoftRemoveMembership clerk id = %q, want orgmem_1", gotClerkMemID)
		}
		// Exactly one member_removed, published inside the same unit of work.
		if len(outbox.published) != 1 {
			t.Fatalf("published %d events, want 1", len(outbox.published))
		}
		ev, ok := outbox.published[0].(MemberRemoved)
		if !ok {
			t.Fatalf("event type = %T, want MemberRemoved", outbox.published[0])
		}
		if ev.Type() != TypeMemberRemoved || ev.AggregateType() != aggregateTypeTenant {
			t.Fatalf("event ids = (%q, %q)", ev.Type(), ev.AggregateType())
		}
		if ev.AggregateID() != tenant.ID || ev.TenantID != tenant.ID || ev.AppUserID != membership.AppUserID {
			t.Fatalf("event payload = %+v", ev)
		}
		if ev.IdempotencyKey() == "" {
			t.Fatal("event idempotency key (event id) is empty")
		}
	})

	t.Run("replay of an already-removed membership publishes nothing", func(t *testing.T) {
		repo := &mockRepo{
			findTenant: func(context.Context, string) (*Tenant, error) { return tenant, nil },
			softRemoveMember: func(_ context.Context, _ database.Tx, _ string) (*Membership, bool, error) {
				return nil, false, nil // nothing was ACTIVE — no-op
			},
		}
		outbox := &recordingOutbox{}
		uow := &fakeUOW{}

		if err := NewUseCase(repo, outbox, uow).OnMembershipRemoved(ctx, "org_abc", "orgmem_1"); err != nil {
			t.Fatalf("OnMembershipRemoved() error = %v", err)
		}
		if !uow.called {
			t.Fatal("unit of work not opened; the idempotent update must still run")
		}
		if len(outbox.published) != 0 {
			t.Fatalf("published %d events on replay, want 0", len(outbox.published))
		}
	})

	t.Run("missing tenant propagates ErrTenantNotFound without opening a tx", func(t *testing.T) {
		repo := &mockRepo{
			findTenant: func(context.Context, string) (*Tenant, error) { return nil, ErrTenantNotFound },
		}
		uow := &fakeUOW{}

		err := NewUseCase(repo, noopOutbox{}, uow).OnMembershipRemoved(ctx, "org_abc", "orgmem_1")
		if !errors.Is(err, ErrTenantNotFound) {
			t.Fatalf("error = %v, want ErrTenantNotFound", err)
		}
		if uow.called {
			t.Fatal("unit of work opened despite the missing tenant")
		}
	})

	t.Run("a publish failure rolls the removal back", func(t *testing.T) {
		repo := &mockRepo{
			findTenant: func(context.Context, string) (*Tenant, error) { return tenant, nil },
			softRemoveMember: func(_ context.Context, _ database.Tx, _ string) (*Membership, bool, error) {
				return membership, true, nil
			},
		}
		outbox := &recordingOutbox{err: errors.New("outbox unreachable")}

		err := NewUseCase(repo, outbox, &fakeUOW{}).OnMembershipRemoved(ctx, "org_abc", "orgmem_1")
		if err == nil {
			t.Fatal("OnMembershipRemoved() error = nil, want the publish failure")
		}
	})
}

func TestUseCase_OnMembershipUpdated(t *testing.T) {
	ctx := context.Background()
	tenant := &Tenant{ID: "tenant-uuid", ClerkOrgID: "org_abc"}

	t.Run("syncs the role under the tenant scope and emits nothing", func(t *testing.T) {
		var got struct {
			clerkMembershipID string
			role              Role
		}
		repo := &mockRepo{
			findTenant: func(context.Context, string) (*Tenant, error) { return tenant, nil },
			updateMemberRole: func(_ context.Context, _ database.Tx, clerkMembershipID string, role Role) error {
				got.clerkMembershipID, got.role = clerkMembershipID, role
				return nil
			},
		}
		outbox := &recordingOutbox{}
		uow := &fakeUOW{}

		if err := NewUseCase(repo, outbox, uow).OnMembershipUpdated(ctx, "org_abc", "orgmem_1", RoleAdmin); err != nil {
			t.Fatalf("OnMembershipUpdated() error = %v", err)
		}
		if uow.scope != tenant.ID {
			t.Fatalf("RLS scope = %q, want %q", uow.scope, tenant.ID)
		}
		if got.clerkMembershipID != "orgmem_1" || got.role != RoleAdmin {
			t.Fatalf("UpdateMembershipRole args = %+v", got)
		}
		if len(outbox.published) != 0 {
			t.Fatalf("published %d events, want 0 (a role change is silent in v0)", len(outbox.published))
		}
	})

	t.Run("missing tenant propagates ErrTenantNotFound without opening a tx", func(t *testing.T) {
		repo := &mockRepo{
			findTenant: func(context.Context, string) (*Tenant, error) { return nil, ErrTenantNotFound },
		}
		uow := &fakeUOW{}

		err := NewUseCase(repo, noopOutbox{}, uow).OnMembershipUpdated(ctx, "org_abc", "orgmem_1", RoleAdmin)
		if !errors.Is(err, ErrTenantNotFound) {
			t.Fatalf("error = %v, want ErrTenantNotFound", err)
		}
		if uow.called {
			t.Fatal("unit of work opened despite the missing tenant")
		}
	})

	t.Run("invalid role short-circuits before any repo call", func(t *testing.T) {
		repo := &mockRepo{
			findTenant: func(context.Context, string) (*Tenant, error) {
				t.Fatal("FindTenantByClerkOrg must not be called on invalid role")
				return nil, nil
			},
		}
		uow := &fakeUOW{}

		err := NewUseCase(repo, noopOutbox{}, uow).OnMembershipUpdated(ctx, "org_abc", "orgmem_1", "MEMBER")
		if !errors.Is(err, ErrInvalidRole) {
			t.Fatalf("error = %v, want ErrInvalidRole", err)
		}
		if uow.called {
			t.Fatal("unit of work opened despite invalid role")
		}
	})
}

func TestUseCase_ResolvePrincipal(t *testing.T) {
	ctx := context.Background()
	tenant := &Tenant{ID: "tenant-uuid", ClerkOrgID: "org_abc"}
	user := &AppUser{ID: "user-uuid", ClerkUserID: "user_xyz", TenantID: "tenant-uuid", Role: RoleAdmin}

	t.Run("assembles the principal from an ACTIVE member + tenant", func(t *testing.T) {
		repo := &mockRepo{
			findTenant:     func(context.Context, string) (*Tenant, error) { return tenant, nil },
			findActiveUser: func(context.Context, string) (*AppUser, error) { return user, nil },
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

	// AC4: no ACTIVE membership — whether the user never existed or was soft-removed,
	// the resolution query returns no row and the repo surfaces ErrUserNotFound, which
	// the auth boundary maps to 401.
	t.Run("a member without an ACTIVE membership is treated as not found", func(t *testing.T) {
		repo := &mockRepo{
			findTenant:     func(context.Context, string) (*Tenant, error) { return tenant, nil },
			findActiveUser: func(context.Context, string) (*AppUser, error) { return nil, ErrUserNotFound },
		}

		_, err := NewUseCase(repo, noopOutbox{}, &fakeUOW{}).ResolvePrincipal(ctx, "user_xyz", "org_abc")
		if !errors.Is(err, ErrUserNotFound) {
			t.Fatalf("error = %v, want ErrUserNotFound", err)
		}
	})

	t.Run("user of another tenant is treated as not found", func(t *testing.T) {
		other := &AppUser{ID: "user-uuid", TenantID: "different-tenant", Role: RoleAdmin}
		repo := &mockRepo{
			findTenant:     func(context.Context, string) (*Tenant, error) { return tenant, nil },
			findActiveUser: func(context.Context, string) (*AppUser, error) { return other, nil },
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

func TestUseCase_GetOrgProfile(t *testing.T) {
	ctx := context.Background()
	const tenantID = "tenant-uuid"

	// AC1: the saved tenant is read by its internal id and returned as-is — the
	// full profile the handler maps to the envelope. No tx is opened (a pool read).
	t.Run("returns the tenant read by its internal id, opening no tx", func(t *testing.T) {
		onboarded := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
		want := &Tenant{
			ID:                    tenantID,
			CNPJ:                  "12345678000195",
			LegalName:             "Escritório LTDA",
			TradeName:             "Escritório",
			Phone:                 "11987654321",
			Email:                 "contato@escritorio.com.br",
			OnboardingCompletedAt: &onboarded,
		}
		var gotTenantID string
		repo := &mockRepo{
			findTenantByID: func(_ context.Context, tid string) (*Tenant, error) {
				gotTenantID = tid
				return want, nil
			},
		}
		uow := &fakeUOW{}

		got, err := NewUseCase(repo, noopOutbox{}, uow).GetOrgProfile(ctx, tenantID)
		if err != nil {
			t.Fatalf("GetOrgProfile() error = %v", err)
		}
		if got != want {
			t.Fatalf("GetOrgProfile() = %+v, want %+v", got, want)
		}
		if gotTenantID != tenantID {
			t.Fatalf("repo read tenant id = %q, want %q (from the principal)", gotTenantID, tenantID)
		}
		if uow.called {
			t.Fatal("unit of work opened for a pool read")
		}
	})

	// AC2: an unknown tenant surfaces ErrTenantNotFound (→ 404 at the edge), never
	// (nil, nil).
	t.Run("missing tenant propagates ErrTenantNotFound", func(t *testing.T) {
		repo := &mockRepo{
			findTenantByID: func(context.Context, string) (*Tenant, error) { return nil, ErrTenantNotFound },
		}

		got, err := NewUseCase(repo, noopOutbox{}, &fakeUOW{}).GetOrgProfile(ctx, tenantID)
		if !errors.Is(err, ErrTenantNotFound) {
			t.Fatalf("error = %v, want ErrTenantNotFound", err)
		}
		if got != nil {
			t.Fatalf("tenant = %+v, want nil on not-found", got)
		}
	})

	t.Run("an infra error from the repo propagates", func(t *testing.T) {
		boom := errors.New("db down")
		repo := &mockRepo{
			findTenantByID: func(context.Context, string) (*Tenant, error) { return nil, boom },
		}

		_, err := NewUseCase(repo, noopOutbox{}, &fakeUOW{}).GetOrgProfile(ctx, tenantID)
		if !errors.Is(err, boom) {
			t.Fatalf("error = %v, want %v", err, boom)
		}
	})
}

func TestUseCase_ListOrgMembers(t *testing.T) {
	ctx := context.Background()
	const tenantID = "tenant-uuid"

	// A plain read model passthrough: the use case forwards the principal's tenant and
	// returns the repo's members verbatim, opening no tx. The ACTIVE-only / tenant filter
	// is enforced in the query (covered by the integration test); here we prove the
	// delegation and scoping.
	t.Run("returns the tenant's members, forwarding the scope and opening no tx", func(t *testing.T) {
		want := []OrgMember{
			{ID: "u-1", Name: "Dra. Ana", Email: "ana@e.com", Role: RoleAdmin},
			{ID: "u-2", Name: "Dr. Bruno", Email: "bruno@e.com", Role: RoleLawyer},
		}
		var gotTenantID string
		repo := &mockRepo{
			listOrgMembers: func(_ context.Context, tid string) ([]OrgMember, error) {
				gotTenantID = tid
				return want, nil
			},
		}
		uow := &fakeUOW{}

		got, err := NewUseCase(repo, noopOutbox{}, uow).ListOrgMembers(ctx, tenantID)
		if err != nil {
			t.Fatalf("ListOrgMembers() error = %v", err)
		}
		if len(got) != 2 || got[0].ID != "u-1" || got[1].Role != RoleLawyer {
			t.Fatalf("members = %+v, want the repo's two rows verbatim", got)
		}
		if gotTenantID != tenantID {
			t.Fatalf("repo scoped to tenant %q, want %q (from the principal)", gotTenantID, tenantID)
		}
		if uow.called {
			t.Fatal("unit of work opened for a pool read")
		}
	})

	t.Run("an infra error from the repo propagates", func(t *testing.T) {
		boom := errors.New("db down")
		repo := &mockRepo{
			listOrgMembers: func(context.Context, string) ([]OrgMember, error) { return nil, boom },
		}

		_, err := NewUseCase(repo, noopOutbox{}, &fakeUOW{}).ListOrgMembers(ctx, tenantID)
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
		Phone:     "11987654321",
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

// mockMembershipGateway is a hand-written membershipGateway double: the RemoveMember
// call delegates to a func field, so each test injects exactly the behavior it needs.
type mockMembershipGateway struct {
	removeMember func(ctx context.Context, clerkOrgID, clerkUserID string) error
	called       bool
	gotOrgID     string
	gotUserID    string
}

func (m *mockMembershipGateway) RemoveMember(ctx context.Context, clerkOrgID, clerkUserID string) error {
	m.called = true
	m.gotOrgID = clerkOrgID
	m.gotUserID = clerkUserID
	return m.removeMember(ctx, clerkOrgID, clerkUserID)
}

func TestUseCase_RemoveMember(t *testing.T) {
	ctx := context.Background()
	tenant := &Tenant{ID: "tenant-uuid", ClerkOrgID: "org_abc"}

	t.Run("revokes the target member on Clerk", func(t *testing.T) {
		repo := &mockRepo{
			findTenantByID: func(context.Context, string) (*Tenant, error) { return tenant, nil },
			findMemberClerk: func(_ context.Context, tenantID, appUserID string) (string, error) {
				if tenantID != tenant.ID || appUserID != "target-uuid" {
					t.Fatalf("lookup scope = (%q, %q), want (%q, target-uuid)", tenantID, appUserID, tenant.ID)
				}
				return "user_target", nil
			},
		}
		gateway := &mockMembershipGateway{removeMember: func(context.Context, string, string) error { return nil }}
		uc := NewUseCase(repo, noopOutbox{}, &fakeUOW{}, WithMembershipGateway(gateway))

		if err := uc.RemoveMember(ctx, tenant.ID, "actor-uuid", "target-uuid"); err != nil {
			t.Fatalf("RemoveMember() error = %v", err)
		}
		if !gateway.called {
			t.Fatal("gateway.RemoveMember was not called")
		}
		if gateway.gotOrgID != tenant.ClerkOrgID || gateway.gotUserID != "user_target" {
			t.Fatalf("gateway called with (%q, %q), want (%q, user_target)", gateway.gotOrgID, gateway.gotUserID, tenant.ClerkOrgID)
		}
	})

	t.Run("an admin cannot remove themself — the gateway is never called", func(t *testing.T) {
		repo := &mockRepo{}
		gateway := &mockMembershipGateway{}
		uc := NewUseCase(repo, noopOutbox{}, &fakeUOW{}, WithMembershipGateway(gateway))

		err := uc.RemoveMember(ctx, tenant.ID, "same-uuid", "same-uuid")
		if !errors.Is(err, ErrCannotRemoveSelf) {
			t.Fatalf("error = %v, want ErrCannotRemoveSelf", err)
		}
		if gateway.called {
			t.Fatal("gateway.RemoveMember was called despite the self-removal guard")
		}
	})

	t.Run("target has no ACTIVE membership in the tenant — typed not-found propagates", func(t *testing.T) {
		repo := &mockRepo{
			findTenantByID: func(context.Context, string) (*Tenant, error) { return tenant, nil },
			findMemberClerk: func(context.Context, string, string) (string, error) {
				return "", ErrUserNotFound
			},
		}
		gateway := &mockMembershipGateway{}
		uc := NewUseCase(repo, noopOutbox{}, &fakeUOW{}, WithMembershipGateway(gateway))

		err := uc.RemoveMember(ctx, tenant.ID, "actor-uuid", "stranger-uuid")
		if !errors.Is(err, ErrUserNotFound) {
			t.Fatalf("error = %v, want ErrUserNotFound", err)
		}
		if gateway.called {
			t.Fatal("gateway.RemoveMember was called despite the lookup failing")
		}
	})

	t.Run("a gateway failure propagates unchanged", func(t *testing.T) {
		repo := &mockRepo{
			findTenantByID:  func(context.Context, string) (*Tenant, error) { return tenant, nil },
			findMemberClerk: func(context.Context, string, string) (string, error) { return "user_target", nil },
		}
		boom := errors.New("clerk unreachable")
		gateway := &mockMembershipGateway{removeMember: func(context.Context, string, string) error { return boom }}
		uc := NewUseCase(repo, noopOutbox{}, &fakeUOW{}, WithMembershipGateway(gateway))

		if err := uc.RemoveMember(ctx, tenant.ID, "actor-uuid", "target-uuid"); !errors.Is(err, boom) {
			t.Fatalf("error = %v, want %v", err, boom)
		}
	})
}

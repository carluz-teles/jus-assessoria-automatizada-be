//go:build integration

// JIT sync integration tests — prove POST /identity/sync's use case against a real
// Postgres: the first sync provisions tenant+user+membership synchronously (no
// webhook) and returns Me with tenant_id; a second sync is idempotent (no duplicate
// rows, no re-emitted events); an invited user joins the existing tenant; and — the
// COALESCE guard — a JIT sync (clerk id NULL) never erases the clerk_membership_id
// the webhook later backfilled. Helpers (readMembership, countMemberships,
// countTenants, readTenant, countMemberJoinedOutbox, countOutboxByType, seedTenant,
// newIdentityUC, newPool) are shared across the integration suite.
package integration_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/jusassessoria/platform/internal/identity"
	"github.com/jusassessoria/platform/internal/notifications"
)

// AC1: the first sync of a brand-new org (the owner) provisions tenant+user+
// membership from the token alone and returns Me with tenant_id set — no pre-seeded
// tenant, no webhook. The membership lands ACTIVE with a NULL clerk id (the JWT
// carries none); the join emits its two events once.
func TestJITSync_FirstOwner_ProvisionsSynchronously(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	const org = "org-jit-owner"
	const orgName = "Escritório JIT"

	uc := newIdentityUC(pool)
	const clerkUser = org + "-user-1"

	// No seedTenant: the tenant is born from the sync alone.
	me, err := uc.Sync(ctx, clerkUser, org, orgName, "ana@b.com", "Ana", identity.RoleAdmin)
	if err != nil {
		t.Fatalf("Sync (tenant absent): %v", err)
	}
	if me.UserID != clerkUser {
		t.Fatalf("Me.UserID = %q, want the clerk id %q", me.UserID, clerkUser)
	}
	if me.TenantID == nil {
		t.Fatal("Me.TenantID is nil, want the freshly provisioned tenant (no webhook)")
	}

	tenantID, name := readTenant(t, pool, org)
	if name != orgName {
		t.Fatalf("provisioned tenant name = %q, want %q", name, orgName)
	}
	if *me.TenantID != tenantID {
		t.Fatalf("Me.TenantID = %q, want the provisioned tenant %q", *me.TenantID, tenantID)
	}

	// The membership is ACTIVE with a NULL clerk id (JIT carries none).
	row := readMembership(t, pool, clerkUser)
	if row.status != string(identity.MembershipActive) || row.role != string(identity.RoleAdmin) {
		t.Fatalf("membership = %+v, want ACTIVE ADMIN", row)
	}
	if row.clerkMemID != nil {
		t.Fatalf("clerk_membership_id = %v, want NULL for a JIT sync", row.clerkMemID)
	}

	// The join emitted its two events exactly once.
	if n := countMemberJoinedOutbox(t, pool, tenantID); n != 1 {
		t.Fatalf("member_joined outbox rows = %d, want 1", n)
	}
	if n := countOutboxByType(t, pool, notifications.TypeNotificationRequested, tenantID); n != 1 {
		t.Fatalf("notification.requested outbox rows = %d, want 1", n)
	}
}

// AC2: a second sync is idempotent against real Postgres — no duplicate tenant, user
// or membership, and member_joined is not re-emitted (the membership is already
// ACTIVE). Me still carries the same tenant_id.
func TestJITSync_Idempotent(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	const org = "org-jit-idem"
	const orgName = "Escritório Idem"

	uc := newIdentityUC(pool)
	const clerkUser = org + "-user-1"

	first, err := uc.Sync(ctx, clerkUser, org, orgName, "ana@b.com", "Ana", identity.RoleAdmin)
	if err != nil {
		t.Fatalf("first Sync: %v", err)
	}
	second, err := uc.Sync(ctx, clerkUser, org, orgName, "ana@b.com", "Ana", identity.RoleAdmin)
	if err != nil {
		t.Fatalf("second Sync: %v", err)
	}
	if first.TenantID == nil || second.TenantID == nil || *first.TenantID != *second.TenantID {
		t.Fatalf("tenant diverged across syncs: %v vs %v", first.TenantID, second.TenantID)
	}

	tenantID := *first.TenantID
	if n := countTenants(t, pool, org); n != 1 {
		t.Fatalf("tenant rows = %d, want 1 (no duplicate)", n)
	}
	if n := countMemberships(t, pool, tenantID); n != 1 {
		t.Fatalf("membership rows = %d, want 1 (no duplicate)", n)
	}
	if n := countMemberJoinedOutbox(t, pool, tenantID); n != 1 {
		t.Fatalf("member_joined outbox rows = %d, want 1 (not re-emitted on replay)", n)
	}
	if n := countOutboxByType(t, pool, notifications.TypeNotificationRequested, tenantID); n != 1 {
		t.Fatalf("notification.requested outbox rows = %d, want 1 (not re-emitted on replay)", n)
	}
}

// AC3: an invited member — the tenant already exists, the user is new with the
// member role — joins the existing tenant. user+membership are created (LAWYER) and
// Me carries the existing tenant_id.
func TestJITSync_InvitedMember_JoinsExistingTenant(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	tenantID := uuid.NewString()
	const org = "org-jit-invite"
	seedTenant(t, pool, tenantID, org, 0)

	uc := newIdentityUC(pool)
	const clerkUser = org + "-invitee"

	me, err := uc.Sync(ctx, clerkUser, org, org, "bob@b.com", "Bob", identity.RoleLawyer)
	if err != nil {
		t.Fatalf("Sync (invited): %v", err)
	}
	if me.TenantID == nil || *me.TenantID != tenantID {
		t.Fatalf("Me.TenantID = %v, want the existing tenant %q", me.TenantID, tenantID)
	}
	if n := countTenants(t, pool, org); n != 1 {
		t.Fatalf("tenant rows = %d, want 1 (invited user reuses the tenant)", n)
	}
	row := readMembership(t, pool, clerkUser)
	if row.status != string(identity.MembershipActive) || row.role != string(identity.RoleLawyer) {
		t.Fatalf("invited membership = %+v, want ACTIVE LAWYER", row)
	}
}

// AC4 (the COALESCE guard): a JIT sync provisions the membership with a NULL clerk
// id; the webhook then backfills the real id; a SECOND JIT sync (NULL again) must NOT
// erase it. Proven against real Postgres — the ON CONFLICT COALESCE keeps the
// backfilled id.
func TestJITSync_COALESCE_PreservesBackfilledClerkID(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	const org = "org-jit-coalesce"
	const orgName = "Escritório Coalesce"

	uc := newIdentityUC(pool)
	const clerkUser = org + "-user-1"
	const clerkMemID = org + "-mem-1" // globally UNIQUE — namespace by org

	// 1) JIT sync: membership created with a NULL clerk id.
	if _, err := uc.Sync(ctx, clerkUser, org, orgName, "ana@b.com", "Ana", identity.RoleAdmin); err != nil {
		t.Fatalf("first Sync: %v", err)
	}
	if row := readMembership(t, pool, clerkUser); row.clerkMemID != nil {
		t.Fatalf("clerk_membership_id after JIT sync = %v, want NULL", row.clerkMemID)
	}

	// 2) The webhook lands and backfills the real clerk id (same org + user).
	if _, err := uc.OnMembershipCreated(ctx, clerkUser, org, orgName, clerkMemID, "ana@b.com", "Ana", identity.RoleAdmin); err != nil {
		t.Fatalf("webhook OnMembershipCreated (backfill): %v", err)
	}
	if row := readMembership(t, pool, clerkUser); row.clerkMemID == nil || *row.clerkMemID != clerkMemID {
		t.Fatalf("clerk_membership_id after webhook = %v, want %q backfilled", row.clerkMemID, clerkMemID)
	}

	// 3) A second JIT sync (NULL again) must NOT erase the backfilled id (COALESCE).
	if _, err := uc.Sync(ctx, clerkUser, org, orgName, "ana@b.com", "Ana", identity.RoleAdmin); err != nil {
		t.Fatalf("second Sync: %v", err)
	}
	if row := readMembership(t, pool, clerkUser); row.clerkMemID == nil || *row.clerkMemID != clerkMemID {
		t.Fatalf("clerk_membership_id after JIT replay = %v, want the backfilled %q preserved", row.clerkMemID, clerkMemID)
	}
}

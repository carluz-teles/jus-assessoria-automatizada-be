//go:build integration

// Identity slice integration tests — prove the onboarding B1 schema against a
// real Postgres: the app_user.phone column exists after the migration and round
// trips through the identity use cases. A user provisioned from membership has a
// NULL phone; a user.updated sync writes the phone; a later phone-less sync
// leaves it untouched (the upsert COALESCE preserves it — at-least-once safety).
package integration_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jusassessoria/platform/internal/identity"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// newIdentityUC wires the identity use case against a real pool, the real
// transactional outbox and the real unit of work.
func newIdentityUC(pool *pgxpool.Pool) *identity.UseCase {
	return identity.NewUseCase(
		identity.NewRepository(pool),
		events.NewOutbox(),
		database.NewUnitOfWork(pool),
	)
}

// appUserPhone reads the raw phone column for a Clerk user id (as owner, RLS
// bypassed). A NULL column comes back as nil.
func appUserPhone(t *testing.T, pool *pgxpool.Pool, clerkUserID string) *string {
	t.Helper()
	var phone *string
	if err := pool.QueryRow(context.Background(),
		`SELECT phone FROM app_user WHERE clerk_user_id = $1`, clerkUserID).Scan(&phone); err != nil {
		t.Fatalf("select phone: %v", err)
	}
	return phone
}

// AC4: phone round-trips through the identity use cases. Provisioning leaves it
// NULL; a user.updated sync writes it; a phone-less sync preserves it.
func TestIdentity_Phone_RoundTrip(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()

	tenantID := uuid.NewString()
	const org = "org-identity-phone"
	seedTenant(t, pool, tenantID, org, 0)

	uc := newIdentityUC(pool)
	const clerkUser = org + "-user-1"

	// Membership provisioning creates the user without a phone → NULL.
	if _, err := uc.ProvisionUser(ctx, clerkUser, org, "ana@b.com", "Ana", identity.RoleLawyer); err != nil {
		t.Fatalf("ProvisionUser: %v", err)
	}
	if got := appUserPhone(t, pool, clerkUser); got != nil {
		t.Fatalf("phone after provisioning = %q, want NULL", *got)
	}

	// user.updated carrying a phone persists it.
	const phone = "+5511987654321"
	if _, err := uc.SyncUser(ctx, clerkUser, "ana@b.com", "Ana", phone); err != nil {
		t.Fatalf("SyncUser with phone: %v", err)
	}
	if got := appUserPhone(t, pool, clerkUser); got == nil || *got != phone {
		t.Fatalf("phone after sync = %v, want %q", got, phone)
	}

	// A later phone-less user.updated must not clear the stored phone (COALESCE).
	if _, err := uc.SyncUser(ctx, clerkUser, "ana@b.com", "Ana", ""); err != nil {
		t.Fatalf("SyncUser without phone: %v", err)
	}
	if got := appUserPhone(t, pool, clerkUser); got == nil || *got != phone {
		t.Fatalf("phone after phone-less sync = %v, want preserved %q", got, phone)
	}
}

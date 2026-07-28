//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestRLS_TenantIsolation proves barrier 2 of tenant isolation (docs §4d.4):
// with app.tenant_id set, a query sees ONLY that tenant's rows; with it unset,
// it sees none.
//
// Correctness detail that makes or breaks this test: `ENABLE ROW LEVEL SECURITY`
// does NOT apply to the table owner or a superuser, and the container's
// connection role IS the owner (it ran the migrations). So the privileged setup
// (insert tenants + users) runs as the owner — RLS bypassed — but every
// assertion runs after `SET LOCAL ROLE app_rls`, a non-owner role that IS
// subject to RLS. This is option (a) from the slice brief: no schema change, no
// FORCE ROW LEVEL SECURITY migration, and it mirrors production, where the app
// should connect as a non-owner role. SET LOCAL scopes the role to the tx.
func TestRLS_TenantIsolation(t *testing.T) {
	pool := newPool(t)

	// --- privileged setup (as owner, RLS bypassed) ----------------------------
	// A non-owner, login-less role the policies actually constrain. Created
	// idempotently so a re-run against a reused database does not fail.
	mustExec(t, pool, `DO $$ BEGIN
		IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_rls') THEN
			CREATE ROLE app_rls;
		END IF;
	END $$`)
	mustExec(t, pool, `GRANT USAGE ON SCHEMA public TO app_rls`)
	mustExec(t, pool, `GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO app_rls`)

	// Two tenants with distinct user counts — a leak would show up as the wrong
	// number, not just non-zero.
	tenantA := uuid.NewString()
	tenantB := uuid.NewString()
	seedTenant(t, pool, tenantA, "org-a", 2)
	seedTenant(t, pool, tenantB, "org-b", 1)

	// --- assertions (as app_rls, subject to RLS) ------------------------------
	tests := []struct {
		name     string
		tenantID string // empty = do not set app.tenant_id at all
		want     int
	}{
		{name: "tenant A sees only its own rows", tenantID: tenantA, want: 2},
		{name: "tenant B sees only its own rows", tenantID: tenantB, want: 1},
		{name: "no tenant set sees nothing", tenantID: "", want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := countAppUsersAsRLSRole(t, tt.tenantID)
			if got != tt.want {
				t.Errorf("app_user count under RLS = %d, want %d", got, tt.want)
			}
		})
	}
}

// mustExec runs a privileged statement (as the owner) and fails the test on error.
func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()

	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// seedTenant inserts a tenant and n app_users for it as the owner (RLS bypassed).
// clerk ids are namespaced by org so the global UNIQUE constraints hold.
func seedTenant(t *testing.T, pool *pgxpool.Pool, tenantID, org string, n int) {
	t.Helper()

	mustExec(t, pool,
		`INSERT INTO tenant (id, clerk_org_id, name) VALUES ($1, $2, $2)`,
		tenantID, org,
	)
	for i := range n {
		mustExec(t, pool,
			`INSERT INTO app_user (clerk_user_id, tenant_id, email) VALUES ($1, $2, $3)`,
			fmt.Sprintf("%s-user-%d", org, i),
			tenantID,
			fmt.Sprintf("u%d@%s.test", i, org),
		)
	}
}

// countAppUsersAsRLSRole counts app_user rows visible to the non-owner app_rls
// role with app.tenant_id set to tenantID (or left unset when empty). It runs
// inside a read-only tx: SET LOCAL ROLE drops to the RLS-bound role and
// set_config applies the tenant scope exactly as lib/database.UnitOfWork does in
// production.
//
// It opens a DEDICATED connection (not a pooled one) on purpose: the first
// set_config('app.tenant_id', …) on a session registers a custom-GUC
// placeholder that persists as an empty string ('') after the tx ends. On a
// reused pooled connection the "no tenant" probe would then read '' — and
// ''::uuid raises 22P02 instead of yielding SQL NULL. A pristine connection has
// never referenced the GUC, so current_setting(…, true) is NULL and the policy
// correctly denies all rows. (A production hardening would wrap the policy in
// NULLIF(current_setting('app.tenant_id', true), '') — noted, out of scope for
// this slice which does not touch the schema.)
func countAppUsersAsRLSRole(t *testing.T, tenantID string) int {
	t.Helper()
	ctx := context.Background()

	conn, err := pgx.Connect(ctx, connString)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)

	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) // read-only probe; never commit

	// Drop to the non-owner role so the policies bind; SET LOCAL keeps it scoped
	// to this transaction.
	if _, err := tx.Exec(ctx, "SET LOCAL ROLE app_rls"); err != nil {
		t.Fatalf("set role: %v", err)
	}

	if tenantID != "" {
		// The same set_config call the UnitOfWork issues (lib/database/uow.go).
		if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
			t.Fatalf("set_config: %v", err)
		}
	}

	var count int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM app_user").Scan(&count); err != nil {
		t.Fatalf("count app_user: %v", err)
	}

	return count
}

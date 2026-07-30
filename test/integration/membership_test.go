//go:build integration

// Membership slice integration tests (convites, fatia F2-1) — prove against a real
// Postgres what the mocked-repo unit tests cannot: an organizationMembership.created
// persists an ACTIVE membership AND the identity.member_joined event in the SAME
// transaction; a replay is idempotent (no duplicate row, no republished event); a
// failed publish rolls the whole join back; the 0005 backfill turns every
// pre-existing app_user into an ACTIVE membership; and the membership RLS policy
// isolates one tenant from another.
package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jusassessoria/platform/internal/identity"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/migrations"
)

// membershipRow is the raw membership as read by the owner (RLS bypassed).
type membershipRow struct {
	role       string
	status     string
	clerkMemID *string
}

// readMembership reads the membership columns for a Clerk user id (joined through
// app_user), so the test asserts what actually landed on the row.
func readMembership(t *testing.T, pool *pgxpool.Pool, clerkUserID string) membershipRow {
	t.Helper()
	var row membershipRow
	if err := pool.QueryRow(context.Background(),
		`SELECT m.role, m.status, m.clerk_membership_id
		   FROM membership m
		   JOIN app_user u ON u.id = m.app_user_id
		  WHERE u.clerk_user_id = $1`, clerkUserID,
	).Scan(&row.role, &row.status, &row.clerkMemID); err != nil {
		t.Fatalf("read membership: %v", err)
	}
	return row
}

// countMemberJoinedOutbox counts unpublished member_joined events for a tenant (the
// outbox aggregate id is the tenant id).
func countMemberJoinedOutbox(t *testing.T, pool *pgxpool.Pool, tenantID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM outbox
		  WHERE type = $1 AND published_at IS NULL AND aggregate_id::text = $2`,
		identity.TypeMemberJoined, tenantID).Scan(&n); err != nil {
		t.Fatalf("count member_joined outbox: %v", err)
	}
	return n
}

// AC2/AC3: organizationMembership.created persists an ACTIVE membership and emits
// member_joined in the same tx; a replay neither duplicates the membership nor
// republishes the event.
func TestIdentity_OnMembershipCreated_PersistsAndOutboxSameTx(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	tenantID := uuid.NewString()
	const org = "org-membership-1"
	seedTenant(t, pool, tenantID, org, 0)

	uc := newIdentityUC(pool)
	const clerkUser = org + "-user-1"

	user, err := uc.OnMembershipCreated(ctx, clerkUser, org, "orgmem-1", "ana@b.com", "Ana", identity.RoleAdmin)
	if err != nil {
		t.Fatalf("OnMembershipCreated: %v", err)
	}

	// The membership row landed ACTIVE, with the mapped role and the clerk id.
	row := readMembership(t, pool, clerkUser)
	if row.status != string(identity.MembershipActive) || row.role != string(identity.RoleAdmin) {
		t.Fatalf("membership row = %+v, want ACTIVE ADMIN", row)
	}
	if row.clerkMemID == nil || *row.clerkMemID != "orgmem-1" {
		t.Fatalf("clerk_membership_id = %v, want orgmem-1", row.clerkMemID)
	}

	// Exactly one member_joined, tied to the tenant, unpublished (same tx).
	if n := countMemberJoinedOutbox(t, pool, tenantID); n != 1 {
		t.Fatalf("member_joined outbox rows = %d, want 1", n)
	}
	var payload []byte
	if err := pool.QueryRow(ctx,
		`SELECT payload FROM outbox WHERE type = $1 AND aggregate_id::text = $2`,
		identity.TypeMemberJoined, tenantID).Scan(&payload); err != nil {
		t.Fatalf("read event payload: %v", err)
	}
	var ev identity.MemberJoined
	if err := json.Unmarshal(payload, &ev); err != nil {
		t.Fatalf("unmarshal member_joined: %v", err)
	}
	if ev.TenantID != tenantID || ev.AppUserID != user.ID || ev.Role != identity.RoleAdmin {
		t.Fatalf("event payload = %+v, unexpected", ev)
	}
	if ev.EventID == "" || ev.AggregateID() != tenantID {
		t.Fatalf("event ids = (%q, %q), want a v7 id and the tenant aggregate", ev.EventID, ev.AggregateID())
	}

	// Replay: the same membership.created must NOT duplicate the row nor republish.
	if _, err := uc.OnMembershipCreated(ctx, clerkUser, org, "orgmem-1", "ana@b.com", "Ana", identity.RoleAdmin); err != nil {
		t.Fatalf("replay OnMembershipCreated: %v", err)
	}
	if n := countMemberships(t, pool, tenantID); n != 1 {
		t.Fatalf("membership rows after replay = %d, want 1", n)
	}
	if n := countMemberJoinedOutbox(t, pool, tenantID); n != 1 {
		t.Fatalf("member_joined outbox rows after replay = %d, want 1 (no republish)", n)
	}
}

// AC2 (atomicity): if the publish fails, the whole unit of work rolls back — no
// membership row AND no app_user survive the join.
func TestIdentity_OnMembershipCreated_PublishFailRollsBackAll(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	tenantID := uuid.NewString()
	const org = "org-membership-rollback"
	seedTenant(t, pool, tenantID, org, 0)

	// failingOutbox (defined in acquisition_test.go) aborts the publish inside the tx.
	uc := identity.NewUseCase(
		identity.NewRepository(pool),
		failingOutbox{err: errors.New("outbox unreachable")},
		database.NewUnitOfWork(pool),
	)

	const clerkUser = org + "-user-1"
	if _, err := uc.OnMembershipCreated(ctx, clerkUser, org, "orgmem-1", "ana@b.com", "Ana", identity.RoleLawyer); err == nil {
		t.Fatal("OnMembershipCreated() error = nil, want the publish failure")
	}

	if n := countMemberships(t, pool, tenantID); n != 0 {
		t.Fatalf("membership rows after rollback = %d, want 0", n)
	}
	var users int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM app_user WHERE clerk_user_id = $1`, clerkUser).Scan(&users); err != nil {
		t.Fatalf("count app_user: %v", err)
	}
	if users != 0 {
		t.Fatalf("app_user rows after rollback = %d, want 0 (join is one tx)", users)
	}
}

// AC1: the 0005 backfill turns every PRE-EXISTING app_user into an ACTIVE
// membership carrying its role. Because the backfill is a one-shot statement that
// runs during the migration, it can only be observed against a database that
// already held app_users when 0005 applied — so this test migrates a throwaway
// database up to 0004, seeds users, then applies 0005 and asserts the memberships.
func TestIdentity_Membership_Backfill(t *testing.T) {
	ctx := context.Background()
	dbName := "backfill_" + strings.ReplaceAll(uuid.NewString(), "-", "")

	// CREATE/DROP DATABASE run on the shared server (autocommit — not in a tx).
	admin := newPool(t)
	mustExec(t, admin, "CREATE DATABASE "+dbName)
	// Registered first so it runs LAST (t.Cleanup is LIFO): the migrator and pool
	// below close before the drop, freeing their connections to the new database.
	t.Cleanup(func() { mustExec(t, admin, "DROP DATABASE "+dbName+" WITH (FORCE)") })

	url := replaceDatabase(t, connString, dbName)
	m := newMigrator(t, url)
	t.Cleanup(func() {
		if srcErr, dbErr := m.Close(); srcErr != nil || dbErr != nil {
			t.Errorf("migrator close: src=%v db=%v", srcErr, dbErr)
		}
	})

	// Migrate up to 0004 (pre-membership), then seed users with distinct roles.
	if err := m.Migrate(4); err != nil {
		t.Fatalf("migrate to 4: %v", err)
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("open pool on %s: %v", dbName, err)
	}
	t.Cleanup(pool.Close)

	tenantID := uuid.NewString()
	const org = "org-backfill"
	mustExec(t, pool, `INSERT INTO tenant (id, clerk_org_id, name) VALUES ($1, $2, $2)`, tenantID, org)
	mustExec(t, pool, `INSERT INTO app_user (clerk_user_id, tenant_id, email, role) VALUES ($1, $2, $3, 'ADMIN')`,
		org+"-owner", tenantID, "owner@"+org+".test")
	mustExec(t, pool, `INSERT INTO app_user (clerk_user_id, tenant_id, email, role) VALUES ($1, $2, $3, 'LAWYER')`,
		org+"-lawyer", tenantID, "lawyer@"+org+".test")

	// Apply 0005 — its backfill runs against the two seeded users.
	if err := m.Up(); err != nil {
		t.Fatalf("apply migration 0005: %v", err)
	}

	// Every seeded user is now an ACTIVE member carrying its role, clerk id NULL.
	if n := countMemberships(t, pool, tenantID); n != 2 {
		t.Fatalf("backfilled membership rows = %d, want 2", n)
	}
	owner := readMembership(t, pool, org+"-owner")
	if owner.status != string(identity.MembershipActive) || owner.role != string(identity.RoleAdmin) || owner.clerkMemID != nil {
		t.Fatalf("owner membership = %+v, want ACTIVE ADMIN with NULL clerk id", owner)
	}
	lawyer := readMembership(t, pool, org+"-lawyer")
	if lawyer.status != string(identity.MembershipActive) || lawyer.role != string(identity.RoleLawyer) {
		t.Fatalf("lawyer membership = %+v, want ACTIVE LAWYER", lawyer)
	}
}

// RLS: the membership table's tenant_isolation policy scopes reads to app.tenant_id,
// exactly like the other tenant tables (barrier 2, docs §4d.4). Mirrors the app_user
// RLS proof in rls_test.go — see its comments for why setup runs as the owner and
// assertions run under the non-owner app_rls role on a pristine connection.
func TestIdentity_Membership_RLS(t *testing.T) {
	pool := newPool(t)

	// The non-owner role the policy constrains, plus grants (idempotent — the
	// suite may create it here or in TestRLS_TenantIsolation, whichever runs first).
	mustExec(t, pool, `DO $$ BEGIN
		IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_rls') THEN
			CREATE ROLE app_rls;
		END IF;
	END $$`)
	mustExec(t, pool, `GRANT USAGE ON SCHEMA public TO app_rls`)
	mustExec(t, pool, `GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO app_rls`)

	tenantA := uuid.NewString()
	tenantB := uuid.NewString()
	seedMembership(t, pool, tenantA, "org-mem-a", 2)
	seedMembership(t, pool, tenantB, "org-mem-b", 1)

	tests := []struct {
		name     string
		tenantID string // empty = do not set app.tenant_id at all
		want     int
	}{
		{name: "tenant A sees only its own memberships", tenantID: tenantA, want: 2},
		{name: "tenant B sees only its own memberships", tenantID: tenantB, want: 1},
		{name: "no tenant set sees nothing", tenantID: "", want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := countMembershipsAsRLSRole(t, tt.tenantID); got != tt.want {
				t.Errorf("membership count under RLS = %d, want %d", got, tt.want)
			}
		})
	}
}

// countMemberships counts membership rows for a tenant as the owner (RLS bypassed).
func countMemberships(t *testing.T, pool *pgxpool.Pool, tenantID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM membership WHERE tenant_id = $1`, tenantID).Scan(&n); err != nil {
		t.Fatalf("count memberships: %v", err)
	}
	return n
}

// seedMembership inserts a tenant with n app_users, each with an ACTIVE membership,
// as the owner (RLS bypassed). clerk ids are namespaced by org for the UNIQUEs.
func seedMembership(t *testing.T, pool *pgxpool.Pool, tenantID, org string, n int) {
	t.Helper()
	mustExec(t, pool, `INSERT INTO tenant (id, clerk_org_id, name) VALUES ($1, $2, $2)`, tenantID, org)
	for i := range n {
		var userID string
		if err := pool.QueryRow(context.Background(),
			`INSERT INTO app_user (clerk_user_id, tenant_id, email, role) VALUES ($1, $2, $3, 'LAWYER') RETURNING id`,
			fmt.Sprintf("%s-user-%d", org, i), tenantID, fmt.Sprintf("u%d@%s.test", i, org),
		).Scan(&userID); err != nil {
			t.Fatalf("insert app_user: %v", err)
		}
		mustExec(t, pool,
			`INSERT INTO membership (tenant_id, app_user_id, role, status) VALUES ($1, $2, 'LAWYER', 'ACTIVE')`,
			tenantID, userID)
	}
}

// countMembershipsAsRLSRole counts membership rows visible to the non-owner app_rls
// role with app.tenant_id scoped to tenantID (or unset when empty), on a dedicated
// pristine connection. See rls_test.go's countAppUsersAsRLSRole for the GUC-pristine
// rationale this mirrors.
func countMembershipsAsRLSRole(t *testing.T, tenantID string) int {
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

	if _, err := tx.Exec(ctx, "SET LOCAL ROLE app_rls"); err != nil {
		t.Fatalf("set role: %v", err)
	}
	if tenantID != "" {
		if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
			t.Fatalf("set_config: %v", err)
		}
	}

	var count int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM membership").Scan(&count); err != nil {
		t.Fatalf("count membership: %v", err)
	}
	return count
}

// newMigrator builds a golang-migrate instance over the embedded migrations bound
// to url. The pgx5:// database driver is registered transitively via lib/database.
func newMigrator(t *testing.T, url string) *migrate.Migrate {
	t.Helper()
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		t.Fatalf("iofs.New: %v", err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, "pgx5://"+strings.TrimPrefix(url, "postgres://"))
	if err != nil {
		t.Fatalf("build migrator: %v", err)
	}
	return m
}

// replaceDatabase rewrites the database name in a postgres connection string,
// preserving host, credentials and query params (e.g. sslmode=disable).
func replaceDatabase(t *testing.T, connStr, dbName string) string {
	t.Helper()
	u, err := url.Parse(connStr)
	if err != nil {
		t.Fatalf("parse conn string: %v", err)
	}
	u.Path = "/" + dbName
	return u.String()
}

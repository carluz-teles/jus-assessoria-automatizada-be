//go:build integration

// Acquisition slice integration tests — prove ActivateIntegration against a real
// Postgres: activating produces ONE DJEN integration row + ONE outbox event in ONE
// transaction (atomic; a failed publish rolls the whole thing back), the upsert emits
// an event only when activation changed, credential_ref is never written, and both the
// app-level tenant filter and RLS isolate one tenant from another.
package integration_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jusassessoria/platform/internal/acquisition"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// newAcquisitionUC wires the acquisition use case against a real pool with the
// real transactional outbox.
func newAcquisitionUC(pool *pgxpool.Pool) *acquisition.UseCase {
	return acquisition.NewUseCase(
		acquisition.NewRepository(pool),
		events.NewOutbox(),
		database.NewUnitOfWork(pool),
	)
}

// countIntegrations counts integration rows for the tenant (as owner, RLS bypassed).
func countIntegrations(t *testing.T, pool *pgxpool.Pool, tenantID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM integration WHERE tenant_id=$1`, tenantID).Scan(&n); err != nil {
		t.Fatalf("count integration: %v", err)
	}
	return n
}

// countActivatedOutbox counts unpublished integration_activated events tied to
// the tenant's integrations (the outbox carries no tenant_id, so join by aggregate).
func countActivatedOutbox(t *testing.T, pool *pgxpool.Pool, tenantID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM outbox
		 WHERE type=$1 AND published_at IS NULL
		   AND aggregate_id::text IN (SELECT id::text FROM integration WHERE tenant_id=$2)`,
		acquisition.TypeIntegrationActivated, tenantID).Scan(&n); err != nil {
		t.Fatalf("count activated outbox: %v", err)
	}
	return n
}

// scope is a small valid scope for the tests.
func scope(oab ...string) acquisition.Scope {
	return acquisition.Scope{OAB: oab}
}

// AC1: activation produces one DJEN integration row AND one outbox event, committed
// together in one transaction.
func TestAcquisition_Activate_RowsAndEventsAtomic(t *testing.T) {
	pool := newPool(t)
	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-acq-1", 0)

	uc := newAcquisitionUC(pool)
	got, err := uc.ActivateIntegration(context.Background(), tenantID, scope("SP123456"))
	if err != nil {
		t.Fatalf("ActivateIntegration() error = %v", err)
	}
	if got == nil {
		t.Fatal("activated integration = nil, want a DJEN row")
	}
	if got.Source != acquisition.SourceDJEN {
		t.Fatalf("activated source = %q, want %q", got.Source, acquisition.SourceDJEN)
	}
	if n := countIntegrations(t, pool, tenantID); n != 1 {
		t.Fatalf("integration rows = %d, want 1", n)
	}
	if n := countActivatedOutbox(t, pool, tenantID); n != 1 {
		t.Fatalf("outbox events = %d, want 1", n)
	}
}

// AC1 (atomicity): if the outbox publish fails, the whole unit of work rolls
// back — no integration row survives.
func TestAcquisition_Activate_PublishFailRollsBackAll(t *testing.T) {
	pool := newPool(t)
	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-acq-2", 0)

	// A publisher that always fails, injected structurally in place of the real
	// outbox, so the first publish aborts the tx.
	uc := acquisition.NewUseCase(
		acquisition.NewRepository(pool),
		failingOutbox{err: errors.New("outbox unreachable")},
		database.NewUnitOfWork(pool),
	)

	_, err := uc.ActivateIntegration(context.Background(), tenantID, scope("SP123456"))
	if err == nil {
		t.Fatal("ActivateIntegration() error = nil, want the publish failure")
	}
	if n := countIntegrations(t, pool, tenantID); n != 0 {
		t.Fatalf("integration rows after rollback = %d, want 0", n)
	}
}

// failingOutbox satisfies the (unexported) outbox port of the use case
// structurally: any type with this Publish signature is accepted by NewUseCase.
type failingOutbox struct{ err error }

func (f failingOutbox) Publish(context.Context, database.Tx, events.Event) error { return f.err }
func (f failingOutbox) PublishBatch(context.Context, database.Tx, []events.Event) error {
	return f.err
}

// AC5: re-activating with a changed scope emits a new event; re-activating with
// the identical scope emits none (and does not duplicate the row).
func TestAcquisition_Activate_Upsert_EventOnlyWhenChanged(t *testing.T) {
	pool := newPool(t)
	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-acq-3", 0)

	uc := newAcquisitionUC(pool)
	ctx := context.Background()

	// First activation: 1 row, 1 event.
	if _, err := uc.ActivateIntegration(ctx, tenantID, scope("SP123456")); err != nil {
		t.Fatalf("first activate: %v", err)
	}
	if n := countIntegrations(t, pool, tenantID); n != 1 {
		t.Fatalf("rows after first = %d, want 1", n)
	}
	if n := countActivatedOutbox(t, pool, tenantID); n != 1 {
		t.Fatalf("events after first = %d, want 1", n)
	}

	// Scope change: still 1 row (upsert), now 2 events.
	if _, err := uc.ActivateIntegration(ctx, tenantID, scope("SP123456", "RJ99")); err != nil {
		t.Fatalf("scope-change activate: %v", err)
	}
	if n := countIntegrations(t, pool, tenantID); n != 1 {
		t.Fatalf("rows after scope change = %d, want 1 (upsert)", n)
	}
	if n := countActivatedOutbox(t, pool, tenantID); n != 2 {
		t.Fatalf("events after scope change = %d, want 2", n)
	}

	// Identical re-post: no new event.
	if _, err := uc.ActivateIntegration(ctx, tenantID, scope("SP123456", "RJ99")); err != nil {
		t.Fatalf("identical activate: %v", err)
	}
	if n := countActivatedOutbox(t, pool, tenantID); n != 2 {
		t.Fatalf("events after identical re-post = %d, want 2 (no new event)", n)
	}
}

// AC10: the request never carries credential_ref, so the column stays NULL.
func TestAcquisition_Activate_CredentialRefAlwaysNull(t *testing.T) {
	pool := newPool(t)
	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-acq-4", 0)

	uc := newAcquisitionUC(pool)
	if _, err := uc.ActivateIntegration(context.Background(), tenantID, scope("SP123456")); err != nil {
		t.Fatalf("ActivateIntegration() error = %v", err)
	}

	var credentialRef *string
	if err := pool.QueryRow(context.Background(),
		`SELECT credential_ref FROM integration WHERE tenant_id=$1 AND source=$2`,
		tenantID, acquisition.SourceDJEN).Scan(&credentialRef); err != nil {
		t.Fatalf("select credential_ref: %v", err)
	}
	if credentialRef != nil {
		t.Fatalf("credential_ref = %q, want NULL", *credentialRef)
	}
}

// AC9: ListIntegrations returns only the calling tenant's rows (app-level filter).
func TestAcquisition_List_ScopedToTenant(t *testing.T) {
	pool := newPool(t)
	tenantA := uuid.NewString()
	tenantB := uuid.NewString()
	seedTenant(t, pool, tenantA, "org-acq-list-a", 0)
	seedTenant(t, pool, tenantB, "org-acq-list-b", 0)

	uc := newAcquisitionUC(pool)
	ctx := context.Background()
	if _, err := uc.ActivateIntegration(ctx, tenantA, scope("SP1")); err != nil {
		t.Fatalf("activate A: %v", err)
	}
	if _, err := uc.ActivateIntegration(ctx, tenantB, scope("RJ2")); err != nil {
		t.Fatalf("activate B: %v", err)
	}

	listA, err := uc.ListIntegrations(ctx, tenantA)
	if err != nil {
		t.Fatalf("ListIntegrations(A): %v", err)
	}
	if len(listA) != 1 {
		t.Fatalf("tenant A list = %d, want 1", len(listA))
	}
	for _, integ := range listA {
		if integ.TenantID != tenantA {
			t.Fatalf("tenant A list leaked a row from tenant %q", integ.TenantID)
		}
	}
}

// AC8: RLS isolates integrations by tenant. As the non-owner app_rls role (the
// only role RLS binds), a query with app.tenant_id set sees only that tenant's
// integrations; unset, it sees none. Mirrors TestRLS_TenantIsolation.
func TestAcquisition_RLS_TenantIsolation(t *testing.T) {
	pool := newPool(t)

	// Ensure the non-owner RLS role exists and can read (idempotent).
	mustExec(t, pool, `DO $$ BEGIN
		IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_rls') THEN
			CREATE ROLE app_rls;
		END IF;
	END $$`)
	mustExec(t, pool, `GRANT USAGE ON SCHEMA public TO app_rls`)
	mustExec(t, pool, `GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO app_rls`)

	tenantA := uuid.NewString()
	tenantB := uuid.NewString()
	seedTenant(t, pool, tenantA, "org-acq-rls-a", 0)
	seedTenant(t, pool, tenantB, "org-acq-rls-b", 0)

	// Seed integrations as the owner (RLS bypassed): 1 DJEN row each.
	uc := newAcquisitionUC(pool)
	ctx := context.Background()
	if _, err := uc.ActivateIntegration(ctx, tenantA, scope("SP1")); err != nil {
		t.Fatalf("activate A: %v", err)
	}
	if _, err := uc.ActivateIntegration(ctx, tenantB, scope("RJ2")); err != nil {
		t.Fatalf("activate B: %v", err)
	}

	tests := []struct {
		name     string
		tenantID string // empty = do not set app.tenant_id
		want     int
	}{
		{name: "tenant A sees only its own", tenantID: tenantA, want: 1},
		{name: "tenant B sees only its own", tenantID: tenantB, want: 1},
		{name: "no tenant set sees nothing", tenantID: "", want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := countIntegrationsAsRLSRole(t, tt.tenantID); got != tt.want {
				t.Errorf("integration count under RLS = %d, want %d", got, tt.want)
			}
		})
	}
}

// countIntegrationsAsRLSRole counts integration rows visible to the non-owner
// app_rls role with app.tenant_id set (or unset when empty). It uses a dedicated
// connection so the custom-GUC placeholder from a prior tx cannot leak an empty
// string into current_setting — see the note in rls_test.go.
func countIntegrationsAsRLSRole(t *testing.T, tenantID string) int {
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
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, "SET LOCAL ROLE app_rls"); err != nil {
		t.Fatalf("set role: %v", err)
	}
	if tenantID != "" {
		if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
			t.Fatalf("set_config: %v", err)
		}
	}

	var count int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM integration").Scan(&count); err != nil {
		t.Fatalf("count integration: %v", err)
	}
	return count
}

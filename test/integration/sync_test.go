//go:build integration

// Sync listener integration tests — prove OnSyncRequested against a real Postgres:
// a first delivery opens a sync_run RUNNING and closes it OK in the same cycle,
// court records and docket entries upsert idempotently by their natural keys, the
// sync_completed event is committed in the same transaction as the OK run, and
// the run is tenant-isolated by RLS. Connector and parser are the slice's stubs
// (fixture: one G1 court record, two docket entries, one intimation).
package integration_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jusassessoria/platform/internal/acquisition"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// newSyncUC wires the sync use case against a real pool, the real transactional
// outbox, and the slice's stub connector/parser.
func newSyncUC(pool *pgxpool.Pool) *acquisition.SyncUseCase {
	return acquisition.NewSyncUseCase(
		acquisition.NewRepository(pool),
		events.NewOutbox(),
		database.NewUnitOfWork(pool),
		acquisition.NewStubConnector(acquisition.SourceDJEN),
		acquisition.NewStubParser(),
	)
}

// syncEvent builds a valid sync_requested event with a fresh event id (so a
// second call is a distinct delivery, not a dedup no-op).
func syncEvent(tenantID, integrationID string) acquisition.SyncRequested {
	return acquisition.SyncRequested{
		Base:          events.Base{EventID: uuid.NewString(), Aggregate: integrationID},
		BackfillJobID: uuid.NewString(),
		TenantID:      tenantID,
		IntegrationID: integrationID,
		SliceIndex:    0,
		WindowFrom:    "2024-01-01",
		WindowTo:      "2024-01-08",
	}
}

func countRows(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	return n
}

// I1: a first delivery opens a sync_run RUNNING and closes it OK with the item
// tallies, all persisted (RUNNING→OK) within the cycle.
func TestSync_FirstDelivery_RunOK(t *testing.T) {
	pool := newPool(t)
	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-sync-1", 0)
	integID := seedIntegration(t, pool, tenantID, acquisition.SourceDJEN)

	if err := newSyncUC(pool).OnSyncRequested(context.Background(), syncEvent(tenantID, integID)); err != nil {
		t.Fatalf("OnSyncRequested() error = %v", err)
	}

	var status string
	var itemsNew, itemsDeduped int
	var finishedAt *string
	if err := pool.QueryRow(context.Background(),
		`SELECT status, items_new, items_deduped, finished_at::text
		 FROM sync_run WHERE integration_id=$1`, integID).
		Scan(&status, &itemsNew, &itemsDeduped, &finishedAt); err != nil {
		t.Fatalf("read sync_run: %v", err)
	}
	if status != acquisition.SyncStatusOK {
		t.Fatalf("sync_run status = %q, want OK", status)
	}
	if itemsNew != 2 || itemsDeduped != 0 {
		t.Fatalf("tallies = {new:%d deduped:%d}, want {2 0}", itemsNew, itemsDeduped)
	}
	if finishedAt == nil {
		t.Fatal("finished_at is NULL on an OK run, want it set")
	}
}

// I2: the court record upsert is idempotent by (tenant, cnj, degree) — two
// distinct deliveries of the same window create exactly one court_record (and one
// court_case).
func TestSync_CourtRecordUpsert_Idempotent(t *testing.T) {
	pool := newPool(t)
	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-sync-2", 0)
	integID := seedIntegration(t, pool, tenantID, acquisition.SourceDJEN)

	uc := newSyncUC(pool)
	ctx := context.Background()
	if err := uc.OnSyncRequested(ctx, syncEvent(tenantID, integID)); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	if err := uc.OnSyncRequested(ctx, syncEvent(tenantID, integID)); err != nil {
		t.Fatalf("second delivery: %v", err)
	}

	if n := countRows(t, pool, `SELECT count(*) FROM court_record WHERE tenant_id=$1`, tenantID); n != 1 {
		t.Fatalf("court_record rows = %d, want 1 (idempotent upsert)", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM court_case WHERE tenant_id=$1`, tenantID); n != 1 {
		t.Fatalf("court_case rows = %d, want 1", n)
	}
}

// I3: docket entries dedup on (court_record_id, hash) — two deliveries of the two
// fixture entries leave exactly two rows, and the second run tallies both as
// deduped.
func TestSync_DocketEntry_OnConflictDoNothing(t *testing.T) {
	pool := newPool(t)
	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-sync-3", 0)
	integID := seedIntegration(t, pool, tenantID, acquisition.SourceDJEN)

	uc := newSyncUC(pool)
	ctx := context.Background()
	if err := uc.OnSyncRequested(ctx, syncEvent(tenantID, integID)); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	if err := uc.OnSyncRequested(ctx, syncEvent(tenantID, integID)); err != nil {
		t.Fatalf("second delivery: %v", err)
	}

	got := countRows(t, pool,
		`SELECT count(*) FROM docket_entry de
		 JOIN court_record cr ON cr.id = de.court_record_id
		 WHERE cr.tenant_id=$1`, tenantID)
	if got != 2 {
		t.Fatalf("docket_entry rows = %d, want 2 (both hashes deduped on re-sync)", got)
	}

	// The second run saw both entries as already present.
	var itemsNew, itemsDeduped int
	if err := pool.QueryRow(context.Background(),
		`SELECT items_new, items_deduped FROM sync_run
		 WHERE integration_id=$1 ORDER BY started_at DESC LIMIT 1`, integID).
		Scan(&itemsNew, &itemsDeduped); err != nil {
		t.Fatalf("read latest sync_run: %v", err)
	}
	if itemsNew != 0 || itemsDeduped != 2 {
		t.Fatalf("second run tallies = {new:%d deduped:%d}, want {0 2}", itemsNew, itemsDeduped)
	}
}

// I4: sync_completed is written in the same transaction as the OK run — after a
// successful cycle, the outbox holds one sync_completed referencing the run,
// plus the observed events (court_record ×1, docket_entry ×2).
func TestSync_SyncCompleted_SameTxAsRun(t *testing.T) {
	pool := newPool(t)
	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-sync-4", 0)
	integID := seedIntegration(t, pool, tenantID, acquisition.SourceDJEN)

	if err := newSyncUC(pool).OnSyncRequested(context.Background(), syncEvent(tenantID, integID)); err != nil {
		t.Fatalf("OnSyncRequested() error = %v", err)
	}

	var syncRunID string
	if err := pool.QueryRow(context.Background(),
		`SELECT id::text FROM sync_run WHERE integration_id=$1`, integID).Scan(&syncRunID); err != nil {
		t.Fatalf("read sync_run id: %v", err)
	}

	completed := countRows(t, pool,
		`SELECT count(*) FROM outbox
		 WHERE type=$1 AND published_at IS NULL AND payload->>'sync_run_id'=$2`,
		acquisition.TypeSyncCompleted, syncRunID)
	if completed != 1 {
		t.Fatalf("sync_completed outbox rows = %d, want 1 (same tx as the OK run)", completed)
	}

	observedRecords := countRows(t, pool,
		`SELECT count(*) FROM outbox
		 WHERE type=$1 AND payload->>'sync_run_id'=$2`,
		acquisition.TypeCourtRecordObserved, syncRunID)
	observedDocket := countRows(t, pool,
		`SELECT count(*) FROM outbox
		 WHERE type=$1 AND payload->>'sync_run_id'=$2`,
		acquisition.TypeDocketEntryObserved, syncRunID)
	if observedRecords != 1 || observedDocket != 2 {
		t.Fatalf("observed events = {court_record:%d docket:%d}, want {1 2}", observedRecords, observedDocket)
	}
}

// I5: sync_run is tenant-isolated by RLS. As the non-owner app_rls role, a query
// with app.tenant_id set sees the run only when it matches the run's tenant.
func TestSync_RLS_TenantIsolation(t *testing.T) {
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
	seedTenant(t, pool, tenantA, "org-sync-rls-a", 0)
	seedTenant(t, pool, tenantB, "org-sync-rls-b", 0)
	integA := seedIntegration(t, pool, tenantA, acquisition.SourceDJEN)

	if err := newSyncUC(pool).OnSyncRequested(context.Background(), syncEvent(tenantA, integA)); err != nil {
		t.Fatalf("sync A: %v", err)
	}

	tests := []struct {
		name     string
		tenantID string // empty = do not set app.tenant_id
		want     int
	}{
		{name: "tenant A sees its own run", tenantID: tenantA, want: 1},
		{name: "tenant B sees no run of A", tenantID: tenantB, want: 0},
		{name: "no tenant set sees nothing", tenantID: "", want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := countSyncRunsAsRLSRole(t, tt.tenantID); got != tt.want {
				t.Errorf("sync_run count under RLS = %d, want %d", got, tt.want)
			}
		})
	}
}

// countSyncRunsAsRLSRole counts sync_run rows visible to the non-owner app_rls
// role with app.tenant_id set (or unset when empty), on a dedicated connection
// (mirrors countBackfillJobsAsRLSRole).
func countSyncRunsAsRLSRole(t *testing.T, tenantID string) int {
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
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM sync_run").Scan(&count); err != nil {
		t.Fatalf("count sync_run: %v", err)
	}
	return count
}

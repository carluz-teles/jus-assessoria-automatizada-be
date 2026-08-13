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
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jusassessoria/platform/internal/acquisition"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// newSyncUC wires the sync use case against a real pool, the real transactional
// outbox, and the slice's stub connectors/parser resolved per source via the
// orchestrator (DJEN + DATAJUD registered, mirroring the worker composition).
func newSyncUC(pool *pgxpool.Pool) *acquisition.SyncUseCase {
	orch := acquisition.NewOrchestrator()
	orch.Register(acquisition.SourceDJEN, acquisition.NewStubConnector(acquisition.SourceDJEN))
	orch.Register(acquisition.SourceDATAJUD, acquisition.NewStubConnector(acquisition.SourceDATAJUD))
	return acquisition.NewSyncUseCase(
		acquisition.NewRepository(pool),
		events.NewOutbox(),
		database.NewUnitOfWork(pool),
		orch,
		acquisition.NewStubParser(),
	)
}

// syncEvent builds a valid sync_requested event with a fresh event id (so a
// second call is a distinct delivery, not a dedup no-op). Source is DJEN so the
// orchestrator resolves a connector.
func syncEvent(t *testing.T, pool *pgxpool.Pool, tenantID, integrationID string) acquisition.SyncRequested {
	return acquisition.SyncRequested{
		Base: events.Base{EventID: uuid.NewString(), Aggregate: integrationID},
		// A REAL backfill_job so sync_run.backfill_job_id's FK holds. Non-empty also
		// keeps the cycle on the backfill path (unlimited — this UC wires no
		// entitlement checker).
		BackfillJobID: seedBackfillJob(t, pool, tenantID, integrationID, 1),
		TenantID:      tenantID,
		IntegrationID: integrationID,
		Source:        acquisition.SourceDJEN,
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

	if err := newSyncUC(pool).OnSyncRequested(context.Background(), syncEvent(t, pool, tenantID, integID)); err != nil {
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
	if err := uc.OnSyncRequested(ctx, syncEvent(t, pool, tenantID, integID)); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	if err := uc.OnSyncRequested(ctx, syncEvent(t, pool, tenantID, integID)); err != nil {
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
	if err := uc.OnSyncRequested(ctx, syncEvent(t, pool, tenantID, integID)); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	if err := uc.OnSyncRequested(ctx, syncEvent(t, pool, tenantID, integID)); err != nil {
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

// I-party: the party materialization is idempotent — two deliveries of the same window
// leave exactly the fixture's parties (2: autor + réu) and advogados (1, on the autor),
// deduped by (tenant, case, role, name) and (tenant, party, oab, uf). Re-observation
// neither duplicates nor breaks dedup. Both rows carry the tenant (RLS-isolated).
func TestSync_Parties_UpsertIdempotent(t *testing.T) {
	pool := newPool(t)
	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-sync-party", 0)
	integID := seedIntegration(t, pool, tenantID, acquisition.SourceDJEN)

	uc := newSyncUC(pool)
	ctx := context.Background()
	if err := uc.OnSyncRequested(ctx, syncEvent(t, pool, tenantID, integID)); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	if err := uc.OnSyncRequested(ctx, syncEvent(t, pool, tenantID, integID)); err != nil {
		t.Fatalf("second delivery: %v", err)
	}

	if n := countRows(t, pool, `SELECT count(*) FROM party WHERE tenant_id=$1`, tenantID); n != 2 {
		t.Fatalf("party rows = %d, want 2 (autor + réu, deduped on re-sync)", n)
	}
	if n := countRows(t, pool,
		`SELECT count(*) FROM party_counsel pc JOIN party p ON p.id = pc.party_id
		 WHERE p.tenant_id=$1`, tenantID); n != 1 {
		t.Fatalf("party_counsel rows = %d, want 1 (the autor's advogado, deduped on re-sync)", n)
	}
	// The counsel hangs off the PLAINTIFF party, resolved to the process's case.
	if n := countRows(t, pool,
		`SELECT count(*) FROM party_counsel pc JOIN party p ON p.id = pc.party_id
		 WHERE p.tenant_id=$1 AND p.role='PLAINTIFF' AND pc.oab='123456' AND pc.uf='SP'`, tenantID); n != 1 {
		t.Fatalf("plaintiff advogado 123456/SP rows = %d, want 1", n)
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

	if err := newSyncUC(pool).OnSyncRequested(context.Background(), syncEvent(t, pool, tenantID, integID)); err != nil {
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

// I5: the sync_run carries the event_id that opened it — the key a re-delivery
// uses to find and resume a run that never closed.
func TestSync_SyncRun_PersistsEventID(t *testing.T) {
	pool := newPool(t)
	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-sync-evt", 0)
	integID := seedIntegration(t, pool, tenantID, acquisition.SourceDJEN)

	ev := syncEvent(t, pool, tenantID, integID)
	if err := newSyncUC(pool).OnSyncRequested(context.Background(), ev); err != nil {
		t.Fatalf("OnSyncRequested() error = %v", err)
	}

	var eventID string
	if err := pool.QueryRow(context.Background(),
		`SELECT event_id FROM sync_run WHERE integration_id=$1`, integID).Scan(&eventID); err != nil {
		t.Fatalf("read sync_run event_id: %v", err)
	}
	if eventID != ev.EventID {
		t.Fatalf("sync_run event_id = %q, want %q", eventID, ev.EventID)
	}
}

// I6: a run left RUNNING by a crashed prior attempt (its dedup mark committed in
// UoW-1, but the close never ran) is RESUMED on re-delivery — the SAME run closes
// OK and no second run is opened. The pre-state is manufactured directly: a
// RUNNING sync_run stamped with the event id, plus the processed_event mark that
// makes SeenOrMark report "already" — exactly what a mid-cycle crash leaves behind.
func TestSync_RedeliveryRunningRun_ResumesAndCloses(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-sync-resume", 0)
	integID := seedIntegration(t, pool, tenantID, acquisition.SourceDJEN)

	ev := syncEvent(t, pool, tenantID, integID)

	// The crashed prior attempt: a RUNNING run tagged with this event id ...
	var stuckRunID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO sync_run (tenant_id, integration_id, connector_id, connector_version, started_at, status, event_id)
		 VALUES ($1, $2, 'stub', 'v0', now(), 'RUNNING', $3) RETURNING id::text`,
		tenantID, integID, ev.EventID).Scan(&stuckRunID); err != nil {
		t.Fatalf("seed stuck sync_run: %v", err)
	}
	// ... and the dedup mark it committed in UoW-1 (consumer const is acquisition.sync).
	mustExec(t, pool,
		`INSERT INTO processed_event (consumer, event_id) VALUES ('acquisition.sync', $1)`, ev.EventID)

	if err := newSyncUC(pool).OnSyncRequested(ctx, ev); err != nil {
		t.Fatalf("OnSyncRequested() error = %v", err)
	}

	// No second run opened; the stuck one closed OK.
	if n := countRows(t, pool, `SELECT count(*) FROM sync_run WHERE integration_id=$1`, integID); n != 1 {
		t.Fatalf("sync_run rows = %d, want 1 (resume reuses the stuck run)", n)
	}
	var id, status string
	if err := pool.QueryRow(ctx,
		`SELECT id::text, status FROM sync_run WHERE integration_id=$1`, integID).Scan(&id, &status); err != nil {
		t.Fatalf("read sync_run: %v", err)
	}
	if id != stuckRunID {
		t.Fatalf("closed run id = %q, want the stuck run %q", id, stuckRunID)
	}
	if status != acquisition.SyncStatusOK {
		t.Fatalf("stuck run status = %q, want OK (resumed and closed)", status)
	}
	// The resume committed the close together with its sync_completed.
	completed := countRows(t, pool,
		`SELECT count(*) FROM outbox WHERE type=$1 AND payload->>'sync_run_id'=$2`,
		acquisition.TypeSyncCompleted, stuckRunID)
	if completed != 1 {
		t.Fatalf("sync_completed outbox rows = %d, want 1 (same tx as the resumed close)", completed)
	}
}

// I7: sync_run is tenant-isolated by RLS. As the non-owner app_rls role, a query
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

	if err := newSyncUC(pool).OnSyncRequested(context.Background(), syncEvent(t, pool, tenantA, integA)); err != nil {
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

// I8: the sync_run close is a compare-and-swap on status=RUNNING, proven against
// real Postgres. Two sequential closes of the SAME run simulate the race: the
// first flips RUNNING → OK and reports closed=true; the second finds the run no
// longer RUNNING, affects zero rows, and reports closed=false without overwriting
// the winner's outcome. This is the guard that keeps the terminal event exactly
// once — the caller publishes only when closed is true.
func TestSync_UpdateSyncRun_CompareAndSwapGuard(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-sync-cas", 0)
	integID := seedIntegration(t, pool, tenantID, acquisition.SourceDJEN)

	// A run left RUNNING by a crashed/in-flight attempt.
	var runID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO sync_run (tenant_id, integration_id, connector_id, connector_version, started_at, status)
		 VALUES ($1, $2, 'stub', 'v0', now(), 'RUNNING') RETURNING id::text`,
		tenantID, integID).Scan(&runID); err != nil {
		t.Fatalf("seed running sync_run: %v", err)
	}

	repo := acquisition.NewRepository(pool)
	uow := database.NewUnitOfWork(pool)
	closeRun := func(status string, itemsNew int) bool {
		var closed bool
		if err := uow.Do(ctx, tenantID, func(tx database.Tx) error {
			var derr error
			closed, derr = repo.UpdateSyncRun(ctx, tx, acquisition.SyncRunOutcome{
				ID: runID, Status: status, ItemsNew: itemsNew, FinishedAt: time.Now(),
			})
			return derr
		}); err != nil {
			t.Fatalf("UpdateSyncRun(%s): %v", status, err)
		}
		return closed
	}

	if !closeRun(acquisition.SyncStatusOK, 7) {
		t.Fatal("first close returned closed=false, want true (RUNNING → OK won the CAS)")
	}
	if closeRun(acquisition.SyncStatusFailed, 0) {
		t.Fatal("second close returned closed=true, want false (run already closed)")
	}

	// The run reflects only the winning (first) close — the loser did not overwrite.
	var status string
	var itemsNew int
	if err := pool.QueryRow(ctx,
		`SELECT status, items_new FROM sync_run WHERE id=$1`, runID).Scan(&status, &itemsNew); err != nil {
		t.Fatalf("read sync_run: %v", err)
	}
	if status != acquisition.SyncStatusOK || itemsNew != 7 {
		t.Fatalf("sync_run = {status:%q items_new:%d}, want {OK 7}", status, itemsNew)
	}
}

// I9: end-to-end, two truly concurrent redeliveries of the same run publish
// sync_completed exactly once. The pre-state is a crash mid-cycle: a RUNNING run
// tagged with the event id plus its committed dedup mark, so BOTH deliveries
// resume the same run and race to close it. The status=RUNNING CAS lets exactly
// one win — one sync_completed (and one of each observed event) in the outbox,
// never two, so the backfill slice it drives is counted exactly once. The court
// record is pre-seeded so the race isolates to the sync_run close.
func TestSync_ConcurrentRedelivery_PublishesCompletedOnce(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-sync-concurrent", 0)
	integID := seedIntegration(t, pool, tenantID, acquisition.SourceDJEN)

	ev := syncEvent(t, pool, tenantID, integID)

	// Pre-seed the fixture's court record so both executions take the find (not
	// create) path — the record's own create-concurrency is a separate concern.
	const cnj = "0000001-11.2024.8.26.0100"
	var caseID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO court_case (tenant_id) VALUES ($1) RETURNING id::text`, tenantID).Scan(&caseID); err != nil {
		t.Fatalf("seed court_case: %v", err)
	}
	mustExec(t, pool,
		`INSERT INTO court_record (tenant_id, case_id, cnj_number, degree, court, completeness)
		 VALUES ($1, $2, $3, 'G1', 'TJSP', 0.5)`, tenantID, caseID, cnj)

	// The crash/in-flight pre-state: a RUNNING run tagged with the event id, plus
	// the dedup mark committed in UoW-1 — so BOTH deliveries resume the same run.
	var runID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO sync_run (tenant_id, integration_id, connector_id, connector_version, started_at, status, event_id)
		 VALUES ($1, $2, 'stub', 'v0', now(), 'RUNNING', $3) RETURNING id::text`,
		tenantID, integID, ev.EventID).Scan(&runID); err != nil {
		t.Fatalf("seed running sync_run: %v", err)
	}
	mustExec(t, pool,
		`INSERT INTO processed_event (consumer, event_id) VALUES ('acquisition.sync', $1)`, ev.EventID)

	uc := newSyncUC(pool)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = uc.OnSyncRequested(ctx, ev)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("execution %d error = %v", i, err)
		}
	}

	// No second run opened; the stuck run closed OK exactly once.
	if n := countRows(t, pool, `SELECT count(*) FROM sync_run WHERE integration_id=$1`, integID); n != 1 {
		t.Fatalf("sync_run rows = %d, want 1", n)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM sync_run WHERE id=$1`, runID).Scan(&status); err != nil {
		t.Fatalf("read sync_run: %v", err)
	}
	if status != acquisition.SyncStatusOK {
		t.Fatalf("sync_run status = %q, want OK", status)
	}

	// Exactly one sync_completed and one of each observed event — only the winner published.
	completed := countRows(t, pool,
		`SELECT count(*) FROM outbox WHERE type=$1 AND payload->>'sync_run_id'=$2`,
		acquisition.TypeSyncCompleted, runID)
	if completed != 1 {
		t.Fatalf("sync_completed outbox rows = %d, want 1 (published once despite the concurrent race)", completed)
	}
	observedRecords := countRows(t, pool,
		`SELECT count(*) FROM outbox WHERE type=$1 AND payload->>'sync_run_id'=$2`,
		acquisition.TypeCourtRecordObserved, runID)
	observedDocket := countRows(t, pool,
		`SELECT count(*) FROM outbox WHERE type=$1 AND payload->>'sync_run_id'=$2`,
		acquisition.TypeDocketEntryObserved, runID)
	if observedRecords != 1 || observedDocket != 2 {
		t.Fatalf("observed events = {court_record:%d docket:%d}, want {1 2}", observedRecords, observedDocket)
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

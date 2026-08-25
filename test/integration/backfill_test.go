//go:build integration

// Backfill listener integration tests — prove OnIntegrationActivated against a
// real Postgres: a first activation creates ONE backfill_job (5 RUNNING slices)
// plus 5 sync_requested outbox rows and one processed_event, all in one atomic
// tx (a failed publish rolls the whole thing back); a duplicate delivery and a
// re-activation are both no-ops; and the job is tenant-isolated by RLS.
package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jusassessoria/platform/internal/acquisition"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// newBackfillUC wires the backfill use case against a real pool and the real
// transactional outbox.
func newBackfillUC(pool *pgxpool.Pool) *acquisition.BackfillUseCase {
	return acquisition.NewBackfillUseCase(
		acquisition.NewRepository(pool),
		events.NewOutbox(),
		database.NewUnitOfWork(pool),
	)
}

// seedIntegration inserts an integration row as the owner (RLS bypassed) and
// returns its id — the backfill_job's FK target.
func seedIntegration(t *testing.T, pool *pgxpool.Pool, tenantID, source string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO integration (tenant_id, source, scope, status)
		 VALUES ($1, $2, '{"oab":["SP1"]}', 'ACTIVE') RETURNING id::text`,
		tenantID, source).Scan(&id); err != nil {
		t.Fatalf("seed integration: %v", err)
	}
	return id
}

// activatedEvent builds a valid integration_activated event with a fresh event id.
// Scope carries "SP1" — the same OAB seedIntegration seeds on the integration row —
// so OnIntegrationActivated's per-OAB AddOrEnableWatchedOAB actually has an OAB to
// upsert (an empty scope now means "no OABs to backfill", not "backfill nothing but
// still fire", since the per-OAB delta drives the whole flow post-fix).
func activatedEvent(tenantID, integrationID string) acquisition.IntegrationActivated {
	return acquisition.IntegrationActivated{
		Base:          events.Base{EventID: uuid.NewString(), Aggregate: integrationID},
		IntegrationID: integrationID,
		TenantID:      tenantID,
		Source:        acquisition.SourceDJEN,
		Scope:         acquisition.Scope{OAB: []string{"SP1"}},
	}
}

func countBackfillJobs(t *testing.T, pool *pgxpool.Pool, tenantID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM backfill_job WHERE tenant_id=$1`, tenantID).Scan(&n); err != nil {
		t.Fatalf("count backfill_job: %v", err)
	}
	return n
}

// countSyncRequestedOutbox counts unpublished sync_requested rows for the tenant
// (the outbox has no tenant_id column, so scope by the payload's tenant_id — it
// is a per-test uuid, so this never sees another test's rows).
func countSyncRequestedOutbox(t *testing.T, pool *pgxpool.Pool, tenantID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM outbox
		 WHERE type=$1 AND published_at IS NULL AND payload->>'tenant_id'=$2`,
		acquisition.TypeSyncRequested, tenantID).Scan(&n); err != nil {
		t.Fatalf("count sync_requested outbox: %v", err)
	}
	return n
}

// countProcessedEvent counts processed_event rows for a specific event id (the
// dedup mark). The id is a per-test uuid, so no cross-test bleed.
func countProcessedEvent(t *testing.T, pool *pgxpool.Pool, eventID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM processed_event WHERE event_id=$1`, eventID).Scan(&n); err != nil {
		t.Fatalf("count processed_event: %v", err)
	}
	return n
}

// AC4: a valid integration_activated event → 1 backfill_job (5 slices, RUNNING)
// + 1 processed_event + 5 sync_requested outbox rows with well-formed payloads.
func TestBackfill_FirstActivation_JobAndSlices(t *testing.T) {
	pool := newPool(t)
	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-bf-1", 0)
	integID := seedIntegration(t, pool, tenantID, acquisition.SourceDJEN)

	ev := activatedEvent(tenantID, integID)
	if err := newBackfillUC(pool).OnIntegrationActivated(context.Background(), ev); err != nil {
		t.Fatalf("OnIntegrationActivated() error = %v", err)
	}

	if n := countBackfillJobs(t, pool, tenantID); n != 1 {
		t.Fatalf("backfill_job rows = %d, want 1", n)
	}
	var slices int
	var status string
	if err := pool.QueryRow(context.Background(),
		`SELECT total_slices, status FROM backfill_job WHERE tenant_id=$1`, tenantID).
		Scan(&slices, &status); err != nil {
		t.Fatalf("read job: %v", err)
	}
	if slices != 5 || status != acquisition.BackfillStatusRunning {
		t.Fatalf("job = {slices:%d status:%q}, want {5 RUNNING}", slices, status)
	}
	if n := countProcessedEvent(t, pool, ev.EventID); n != 1 {
		t.Fatalf("processed_event rows = %d, want 1", n)
	}
	if n := countSyncRequestedOutbox(t, pool, tenantID); n != 5 {
		t.Fatalf("sync_requested outbox rows = %d, want 5", n)
	}

	// The slice-0 payload carries the identifiers and dated bounds a consumer needs.
	var payload []byte
	if err := pool.QueryRow(context.Background(),
		`SELECT payload FROM outbox
		 WHERE type=$1 AND payload->>'tenant_id'=$2 AND payload->>'slice_index'='0'`,
		acquisition.TypeSyncRequested, tenantID).Scan(&payload); err != nil {
		t.Fatalf("read slice-0 payload: %v", err)
	}
	var sr acquisition.SyncRequested
	if err := json.Unmarshal(payload, &sr); err != nil {
		t.Fatalf("unmarshal sync_requested: %v", err)
	}
	if sr.BackfillJobID == "" || sr.IntegrationID != integID || sr.TenantID != tenantID || sr.SliceIndex != 0 {
		t.Fatalf("slice-0 payload = %+v, unexpected", sr)
	}
	if _, err := time.Parse("2006-01-02", sr.WindowFrom); err != nil {
		t.Fatalf("window_from = %q, not a date: %v", sr.WindowFrom, err)
	}
	if _, err := time.Parse("2006-01-02", sr.WindowTo); err != nil {
		t.Fatalf("window_to = %q, not a date: %v", sr.WindowTo, err)
	}
}

// AC4 (atomicity): if a publish fails, the whole unit of work rolls back — no
// job, no outbox row, and no dedup mark survive.
func TestBackfill_PublishFailRollsBackAll(t *testing.T) {
	pool := newPool(t)
	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-bf-rollback", 0)
	integID := seedIntegration(t, pool, tenantID, acquisition.SourceDJEN)

	// failingOutbox (defined in acquisition_test.go) aborts the first publish,
	// after the job insert has already run inside the tx.
	uc := acquisition.NewBackfillUseCase(
		acquisition.NewRepository(pool),
		failingOutbox{err: errors.New("outbox unreachable")},
		database.NewUnitOfWork(pool),
	)

	ev := activatedEvent(tenantID, integID)
	if err := uc.OnIntegrationActivated(context.Background(), ev); err == nil {
		t.Fatal("OnIntegrationActivated() error = nil, want the publish failure")
	}

	if n := countBackfillJobs(t, pool, tenantID); n != 0 {
		t.Fatalf("backfill_job rows after rollback = %d, want 0", n)
	}
	if n := countSyncRequestedOutbox(t, pool, tenantID); n != 0 {
		t.Fatalf("sync_requested rows after rollback = %d, want 0", n)
	}
	if n := countProcessedEvent(t, pool, ev.EventID); n != 0 {
		t.Fatalf("processed_event rows after rollback = %d, want 0", n)
	}
}

// AC5: the same event delivered twice — SeenOrMark reports seen on the 2nd, so
// no second job or slice is produced and the call acks without error.
func TestBackfill_DuplicateDelivery_NoOp(t *testing.T) {
	pool := newPool(t)
	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-bf-dup", 0)
	integID := seedIntegration(t, pool, tenantID, acquisition.SourceDJEN)

	uc := newBackfillUC(pool)
	ctx := context.Background()
	ev := activatedEvent(tenantID, integID)

	if err := uc.OnIntegrationActivated(ctx, ev); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	if err := uc.OnIntegrationActivated(ctx, ev); err != nil {
		t.Fatalf("second (duplicate) delivery: %v", err)
	}

	if n := countBackfillJobs(t, pool, tenantID); n != 1 {
		t.Fatalf("backfill_job rows = %d, want 1 (duplicate must not add one)", n)
	}
	if n := countSyncRequestedOutbox(t, pool, tenantID); n != 5 {
		t.Fatalf("sync_requested rows = %d, want 5 (duplicate must not add more)", n)
	}
	if n := countProcessedEvent(t, pool, ev.EventID); n != 1 {
		t.Fatalf("processed_event rows = %d, want 1", n)
	}
}

// AC6: a second activation (a NEW event id) for the SAME integration is guarded
// by the existing job — the new event is marked processed but produces no second
// job or slice.
func TestBackfill_Reactivation_Guarded(t *testing.T) {
	pool := newPool(t)
	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-bf-react", 0)
	integID := seedIntegration(t, pool, tenantID, acquisition.SourceDJEN)

	uc := newBackfillUC(pool)
	ctx := context.Background()
	first := activatedEvent(tenantID, integID)
	second := activatedEvent(tenantID, integID) // fresh event id, same integration

	if err := uc.OnIntegrationActivated(ctx, first); err != nil {
		t.Fatalf("first activation: %v", err)
	}
	if err := uc.OnIntegrationActivated(ctx, second); err != nil {
		t.Fatalf("second activation: %v", err)
	}

	if n := countBackfillJobs(t, pool, tenantID); n != 1 {
		t.Fatalf("backfill_job rows = %d, want 1 (re-activation must not add one)", n)
	}
	if n := countSyncRequestedOutbox(t, pool, tenantID); n != 5 {
		t.Fatalf("sync_requested rows = %d, want 5 (re-activation must not add more)", n)
	}
	// Both distinct events are marked processed (the guard still acks the 2nd).
	if n := countProcessedEvent(t, pool, second.EventID); n != 1 {
		t.Fatalf("second event processed_event rows = %d, want 1", n)
	}
}

// AC7: the job is created under the payload's tenant and RLS isolates it — the
// non-owner app_rls role sees the job only when app.tenant_id matches.
func TestBackfill_RLS_TenantIsolation(t *testing.T) {
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
	seedTenant(t, pool, tenantA, "org-bf-rls-a", 0)
	seedTenant(t, pool, tenantB, "org-bf-rls-b", 0)
	integA := seedIntegration(t, pool, tenantA, acquisition.SourceDJEN)

	// Create the job for tenant A (owner insert; RLS bypassed on write).
	if err := newBackfillUC(pool).OnIntegrationActivated(context.Background(),
		activatedEvent(tenantA, integA)); err != nil {
		t.Fatalf("activate A: %v", err)
	}

	tests := []struct {
		name     string
		tenantID string // empty = do not set app.tenant_id
		want     int
	}{
		{name: "tenant A sees its own job", tenantID: tenantA, want: 1},
		{name: "tenant B sees no job of A", tenantID: tenantB, want: 0},
		{name: "no tenant set sees nothing", tenantID: "", want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := countBackfillJobsAsRLSRole(t, tt.tenantID); got != tt.want {
				t.Errorf("backfill_job count under RLS = %d, want %d", got, tt.want)
			}
		})
	}
}

// countBackfillJobsAsRLSRole counts backfill_job rows visible to the non-owner
// app_rls role with app.tenant_id set (or unset when empty), on a dedicated
// connection (mirrors countIntegrationsAsRLSRole in acquisition_test.go).
func countBackfillJobsAsRLSRole(t *testing.T, tenantID string) int {
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
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM backfill_job").Scan(&count); err != nil {
		t.Fatalf("count backfill_job: %v", err)
	}
	return count
}

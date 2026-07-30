//go:build integration

// Backfill completion-counter integration tests — prove the "last slice turns off
// the light" counter against a real Postgres: closing every slice of a job (a mix
// of OK and FAILED) sums the counters correctly and finalizes the job EXACTLY once
// with the right terminal status (COMPLETED when no slice failed, PARTIAL when at
// least one did); the finalize + backfill_finished commit in ONE transaction (a
// failed publish rolls back the whole close, leaving the job RUNNING); and a
// mismatched tenant is blocked by RLS to a no-op.
package integration_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jusassessoria/platform/internal/acquisition"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// seedBackfillJob inserts a RUNNING backfill_job with total slices as the owner
// (RLS bypassed) and returns its id — the counter's target.
func seedBackfillJob(t *testing.T, pool *pgxpool.Pool, tenantID, integrationID string, total int) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO backfill_job
		     (tenant_id, integration_id, window_from, window_to, total_slices, status)
		 VALUES ($1, $2, '2024-01-01', '2025-01-01', $3, 'RUNNING')
		 RETURNING id::text`,
		tenantID, integrationID, total).Scan(&id); err != nil {
		t.Fatalf("seed backfill_job: %v", err)
	}
	return id
}

// readBackfillJob reads a job's counters and status as the owner (RLS bypassed).
func readBackfillJob(t *testing.T, pool *pgxpool.Pool, jobID string) (slicesOK, slicesError int, status string) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT slices_ok, slices_error, status FROM backfill_job WHERE id=$1`, jobID).
		Scan(&slicesOK, &slicesError, &status); err != nil {
		t.Fatalf("read backfill_job: %v", err)
	}
	return slicesOK, slicesError, status
}

// countBackfillFinished counts unpublished backfill_finished outbox rows for a job.
func countBackfillFinished(t *testing.T, pool *pgxpool.Pool, jobID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM outbox
		 WHERE type=$1 AND published_at IS NULL AND payload->>'backfill_job_id'=$2`,
		acquisition.TypeBackfillFinished, jobID).Scan(&n); err != nil {
		t.Fatalf("count backfill_finished: %v", err)
	}
	return n
}

// sliceCompletedEvent / sliceFailedEvent build one slice-close event with a fresh
// event id (so each is a distinct delivery, not a dedup no-op).
func sliceCompletedEvent(tenantID, jobID, integrationID string) acquisition.SyncCompleted {
	return acquisition.SyncCompleted{
		Base:          events.Base{EventID: uuid.NewString(), Aggregate: uuid.NewString()},
		TenantID:      tenantID,
		SyncRunID:     uuid.NewString(),
		IntegrationID: integrationID,
		BackfillJobID: jobID,
	}
}

func sliceFailedEvent(tenantID, jobID, integrationID string) acquisition.SyncFailed {
	return acquisition.SyncFailed{
		Base:          events.Base{EventID: uuid.NewString(), Aggregate: uuid.NewString()},
		TenantID:      tenantID,
		SyncRunID:     uuid.NewString(),
		IntegrationID: integrationID,
		BackfillJobID: jobID,
		Reason:        "fetch timeout",
	}
}

// I1a: closing every slice with no failure sums the counters and finalizes the job
// COMPLETED exactly once.
func TestBackfillCompletion_AllOK_CompletesOnce(t *testing.T) {
	pool := newPool(t)
	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-bfc-ok", 0)
	integID := seedIntegration(t, pool, tenantID, acquisition.SourceDJEN)
	jobID := seedBackfillJob(t, pool, tenantID, integID, 2)

	uc := newBackfillUC(pool)
	ctx := context.Background()
	if err := uc.OnSyncCompleted(ctx, sliceCompletedEvent(tenantID, jobID, integID)); err != nil {
		t.Fatalf("close slice 1: %v", err)
	}
	if err := uc.OnSyncCompleted(ctx, sliceCompletedEvent(tenantID, jobID, integID)); err != nil {
		t.Fatalf("close slice 2 (last): %v", err)
	}

	ok, errc, status := readBackfillJob(t, pool, jobID)
	if ok != 2 || errc != 0 || status != acquisition.BackfillStatusCompleted {
		t.Fatalf("job = {ok:%d err:%d status:%q}, want {2 0 COMPLETED}", ok, errc, status)
	}
	if n := countBackfillFinished(t, pool, jobID); n != 1 {
		t.Fatalf("backfill_finished rows = %d, want 1 (finalize exactly once)", n)
	}
}

// I1b: a mix of OK and FAILED slices sums correctly and finalizes PARTIAL exactly
// once — the last close (whichever it is) wins the RUNNING → terminal transition.
func TestBackfillCompletion_Mixed_PartialOnce(t *testing.T) {
	pool := newPool(t)
	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-bfc-mix", 0)
	integID := seedIntegration(t, pool, tenantID, acquisition.SourceDJEN)
	jobID := seedBackfillJob(t, pool, tenantID, integID, 3)

	uc := newBackfillUC(pool)
	ctx := context.Background()
	if err := uc.OnSyncCompleted(ctx, sliceCompletedEvent(tenantID, jobID, integID)); err != nil {
		t.Fatalf("close slice 1 (ok): %v", err)
	}
	if err := uc.OnSyncFailed(ctx, sliceFailedEvent(tenantID, jobID, integID)); err != nil {
		t.Fatalf("close slice 2 (failed): %v", err)
	}
	if err := uc.OnSyncCompleted(ctx, sliceCompletedEvent(tenantID, jobID, integID)); err != nil {
		t.Fatalf("close slice 3 (last, ok): %v", err)
	}

	ok, errc, status := readBackfillJob(t, pool, jobID)
	if ok != 2 || errc != 1 || status != acquisition.BackfillStatusPartial {
		t.Fatalf("job = {ok:%d err:%d status:%q}, want {2 1 PARTIAL}", ok, errc, status)
	}
	if n := countBackfillFinished(t, pool, jobID); n != 1 {
		t.Fatalf("backfill_finished rows = %d, want 1 (finalize exactly once)", n)
	}
}

// I2: the finalize and its backfill_finished commit in ONE transaction. With a
// publisher that always fails, closing the last slice aborts the whole close: the
// counter increment AND the status flip roll back, leaving the job RUNNING with no
// finished event.
func TestBackfillCompletion_PublishFailRollsBackFinalize(t *testing.T) {
	pool := newPool(t)
	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-bfc-rollback", 0)
	integID := seedIntegration(t, pool, tenantID, acquisition.SourceDJEN)
	jobID := seedBackfillJob(t, pool, tenantID, integID, 2)

	ctx := context.Background()

	// Slice 1 (non-final) publishes nothing, so it commits even with a failing
	// outbox: real outbox for the first close.
	if err := newBackfillUC(pool).OnSyncCompleted(ctx, sliceCompletedEvent(tenantID, jobID, integID)); err != nil {
		t.Fatalf("close slice 1: %v", err)
	}
	if ok, _, status := readBackfillJob(t, pool, jobID); ok != 1 || status != acquisition.BackfillStatusRunning {
		t.Fatalf("after slice 1: {ok:%d status:%q}, want {1 RUNNING}", ok, status)
	}

	// Slice 2 is the finalizing close: it finalizes AND publishes, so a failing
	// publish must roll the whole close back (increment + status flip).
	failing := acquisition.NewBackfillUseCase(
		acquisition.NewRepository(pool),
		failingOutbox{err: errors.New("outbox unreachable")},
		database.NewUnitOfWork(pool),
	)
	if err := failing.OnSyncCompleted(ctx, sliceCompletedEvent(tenantID, jobID, integID)); err == nil {
		t.Fatal("close last slice: error = nil, want the publish failure")
	}

	ok, errc, status := readBackfillJob(t, pool, jobID)
	if ok != 1 || errc != 0 || status != acquisition.BackfillStatusRunning {
		t.Fatalf("after rolled-back finalize: {ok:%d err:%d status:%q}, want {1 0 RUNNING}", ok, errc, status)
	}
	if n := countBackfillFinished(t, pool, jobID); n != 0 {
		t.Fatalf("backfill_finished rows after rollback = %d, want 0", n)
	}
}

// I3: the app-level tenant filter (isolation barrier 1) isolates the counter. A
// sync_completed carrying the WRONG tenant for a job cannot touch it — the
// increment's WHERE tenant_id=$ matches zero rows, so the close is a no-op ack:
// the job's counters and status are untouched and no finished event is emitted.
// (Barrier 2, the RLS policy itself, is proven separately for reads in
// TestBackfill_RLS_TenantIsolation; the owner pool here bypasses RLS, so this
// exercises the WHERE-clause barrier the counter relies on.)
func TestBackfillCompletion_RLS_WrongTenantNoOp(t *testing.T) {
	pool := newPool(t)
	tenantA := uuid.NewString()
	tenantB := uuid.NewString()
	seedTenant(t, pool, tenantA, "org-bfc-rls-a", 0)
	seedTenant(t, pool, tenantB, "org-bfc-rls-b", 0)
	integA := seedIntegration(t, pool, tenantA, acquisition.SourceDJEN)
	jobA := seedBackfillJob(t, pool, tenantA, integA, 3)

	// Deliver a close for tenant A's job, but under tenant B's scope.
	if err := newBackfillUC(pool).OnSyncCompleted(context.Background(),
		sliceCompletedEvent(tenantB, jobA, integA)); err != nil {
		t.Fatalf("wrong-tenant close: error = %v, want nil (no-op ack)", err)
	}

	ok, errc, status := readBackfillJob(t, pool, jobA)
	if ok != 0 || errc != 0 || status != acquisition.BackfillStatusRunning {
		t.Fatalf("job A after wrong-tenant close = {ok:%d err:%d status:%q}, want {0 0 RUNNING} (untouched)", ok, errc, status)
	}
	if n := countBackfillFinished(t, pool, jobA); n != 0 {
		t.Fatalf("backfill_finished rows = %d, want 0 (RLS blocked the close)", n)
	}
}

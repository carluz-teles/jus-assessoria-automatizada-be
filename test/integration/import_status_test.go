//go:build integration

// Import-status read integration test — proves acquisition.Repository.GetImportStatus
// against a real Postgres: a RUNNING backfill_job reads back as importing; a newer
// terminal job wins; a tenant with no job gets NONE (banner hidden); and the read is
// tenant-scoped (barrier 1), so one tenant never sees another's import.
package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jusassessoria/platform/internal/acquisition"
)

// seedImportJob inserts one backfill_job as the owner (RLS bypassed) with an explicit
// created_at, so "newest job wins" is deterministic.
func seedImportJob(t *testing.T, pool *pgxpool.Pool, tenantID, integrationID, status string, total, ok, errCount int, createdAt time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO backfill_job
		   (tenant_id, integration_id, window_from, window_to, total_slices, slices_ok, slices_error, status, created_at)
		 VALUES ($1, $2, '2025-01-01', '2026-01-01', $3, $4, $5, $6, $7)`,
		tenantID, integrationID, total, ok, errCount, status, createdAt); err != nil {
		t.Fatalf("seed backfill_job: %v", err)
	}
}

func TestAcquisition_ImportStatus_RunningNewestWinsAndNone(t *testing.T) {
	pool := newPool(t)
	repo := acquisition.NewRepository(pool)
	ctx := context.Background()

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-import-status", 0)
	integrationID := seedIntegration(t, pool, tenantID, acquisition.SourceDJEN)

	// No backfill_job yet → NONE, not importing (banner stays hidden).
	st, err := repo.GetImportStatus(ctx, tenantID)
	if err != nil {
		t.Fatalf("GetImportStatus (none): %v", err)
	}
	if st.Importing || st.Status != "NONE" {
		t.Fatalf("no job: got %+v, want {Importing:false, Status:NONE}", st)
	}

	// A RUNNING job → importing, with the tallies.
	base := time.Now().UTC().Truncate(time.Second)
	seedImportJob(t, pool, tenantID, integrationID, "RUNNING", 53, 10, 0, base)
	st, err = repo.GetImportStatus(ctx, tenantID)
	if err != nil {
		t.Fatalf("GetImportStatus (running): %v", err)
	}
	if !st.Importing || st.Status != "RUNNING" || st.TotalSlices != 53 || st.SlicesOK != 10 {
		t.Fatalf("running: got %+v, want importing RUNNING 53/10", st)
	}

	// A NEWER terminal job wins → not importing anymore.
	seedImportJob(t, pool, tenantID, integrationID, "COMPLETED", 53, 53, 0, base.Add(time.Minute))
	st, err = repo.GetImportStatus(ctx, tenantID)
	if err != nil {
		t.Fatalf("GetImportStatus (completed): %v", err)
	}
	if st.Importing || st.Status != "COMPLETED" {
		t.Fatalf("newest wins: got %+v, want not-importing COMPLETED", st)
	}
}

func TestAcquisition_ImportStatus_TenantIsolation(t *testing.T) {
	pool := newPool(t)
	repo := acquisition.NewRepository(pool)
	ctx := context.Background()

	tenantA := uuid.NewString()
	tenantB := uuid.NewString()
	seedTenant(t, pool, tenantA, "org-import-a", 0)
	seedTenant(t, pool, tenantB, "org-import-b", 0)
	intA := seedIntegration(t, pool, tenantA, acquisition.SourceDJEN)
	seedImportJob(t, pool, tenantA, intA, "RUNNING", 10, 1, 0, time.Now().UTC())

	// Tenant B has no job of its own and never sees tenant A's (barrier 1: the query
	// filters tenant_id).
	st, err := repo.GetImportStatus(ctx, tenantB)
	if err != nil {
		t.Fatalf("GetImportStatus (tenant B): %v", err)
	}
	if st.Importing || st.Status != "NONE" {
		t.Fatalf("tenant isolation leak: B got %+v, want NONE", st)
	}
}

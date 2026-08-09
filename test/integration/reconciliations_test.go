//go:build integration

// Reconciliations read-model integration tests — prove GET /v1/acquisition/
// reconciliations' backing read against a REAL Postgres: the recent sync_run
// executions (newest first, window bounds, error message lifted from the jsonb),
// the acquired totals, the import state, and tenant isolation.
package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jusassessoria/platform/internal/acquisition"
)

// seedSyncRun inserts one sync_run as the owner (RLS bypassed). window/finishedAt
// empty → NULL; errMsg non-empty → the {"message": …} jsonb the write path records.
func seedSyncRun(
	t *testing.T,
	pool *pgxpool.Pool,
	tenantID, integrationID, status, windowFrom, windowTo, errMsg string,
	itemsNew, itemsDeduped int,
	startedAt time.Time,
	finished bool,
) {
	t.Helper()
	var window any
	var windowEnd any
	if windowFrom != "" {
		window, windowEnd = windowFrom, windowTo
	}
	var errJSON any
	if errMsg != "" {
		errJSON = map[string]string{"message": errMsg}
	}
	var finishedAt any
	if finished {
		finishedAt = startedAt.Add(time.Minute)
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO sync_run
		   (tenant_id, integration_id, connector_id, connector_version, started_at,
		    finished_at, status, items_new, items_deduped, error, window_from, window_to)
		 VALUES ($1, $2, 'djen', 'v1', $3, $4, $5, $6, $7, $8, $9, $10)`,
		tenantID, integrationID, startedAt, finishedAt, status,
		itemsNew, itemsDeduped, errJSON, window, windowEnd); err != nil {
		t.Fatalf("seed sync_run: %v", err)
	}
}

// seedIntimation inserts one intimation for the record (owner insert) — enough to
// count in the reconciliations totals.
func seedIntimation(t *testing.T, pool *pgxpool.Pool, tenantID string, cr *acquisition.CourtRecord) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO intimation
		   (tenant_id, case_id, court_record_id, hash, made_available_at, published_at,
		    deadline_start_at, content, source)
		 VALUES ($1, $2, $3, $4, '2026-01-05', '2026-01-06', '2026-01-07', 'teor', 'DJEN')`,
		tenantID, cr.CaseID, cr.ID, uuid.NewString()); err != nil {
		t.Fatalf("seed intimation: %v", err)
	}
}

// The whole view: import state from the latest backfill_job, totals from the
// acervo, runs newest-first with window bounds and the FAILED run's message —
// and nothing leaked from another tenant.
func TestAcquisition_Reconciliations_View(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-recon", 0)
	integrationID := seedIntegration(t, pool, tenantID, acquisition.SourceDJEN)

	otherTenant := uuid.NewString()
	seedTenant(t, pool, otherTenant, "org-recon-other", 0)
	otherIntegration := seedIntegration(t, pool, otherTenant, acquisition.SourceDJEN)

	// Import: one RUNNING backfill_job → importing=true.
	seedImportJob(t, pool, tenantID, integrationID, "RUNNING", 53, 19, 2, time.Now())

	// Totals: one ACTIVE record + one intimation (limit high enough to create).
	cr, err := findOrCreateCourtRecord(t, pool, tenantID, "0000001-11.2026.8.26.0001", 10)
	if err != nil {
		t.Fatalf("seed court record: %v", err)
	}
	seedIntimation(t, pool, tenantID, cr)

	// Runs, oldest → newest: OK with window+items, FAILED with error, RUNNING open.
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	seedSyncRun(t, pool, tenantID, integrationID, "OK", "2026-07-01", "2026-07-08", "", 96, 4, base, true)
	seedSyncRun(t, pool, tenantID, integrationID, "FAILED", "2026-07-08", "2026-07-15", "boom", 0, 0, base.Add(time.Hour), true)
	seedSyncRun(t, pool, tenantID, integrationID, "RUNNING", "2026-07-15", "2026-07-22", "", 0, 0, base.Add(2*time.Hour), false)

	// Another tenant's run must never surface.
	seedSyncRun(t, pool, otherTenant, otherIntegration, "OK", "2026-07-01", "2026-07-08", "", 999, 0, base, true)

	uc := acquisition.NewReadUseCase(acquisition.NewRepository(pool))
	view, err := uc.Reconciliations(ctx, tenantID)
	if err != nil {
		t.Fatalf("Reconciliations: %v", err)
	}

	if !view.Import.Importing || view.Import.Status != "RUNNING" {
		t.Fatalf("import = %+v, want importing RUNNING", view.Import)
	}
	if view.Import.SlicesOK != 19 || view.Import.SlicesError != 2 || view.Import.TotalSlices != 53 {
		t.Fatalf("import tallies = %+v, want 19/2/53", view.Import)
	}
	if view.Totals.CourtRecords != 1 || view.Totals.Intimations != 1 {
		t.Fatalf("totals = %+v, want 1 processo / 1 intimação", view.Totals)
	}

	if len(view.Runs) != 3 {
		t.Fatalf("runs = %d, want 3 (another tenant's run must not leak)", len(view.Runs))
	}
	running, failed, ok := view.Runs[0], view.Runs[1], view.Runs[2]

	if running.Status != "RUNNING" || running.FinishedAt != nil || running.Error != nil {
		t.Errorf("runs[0] = %+v, want an open RUNNING run (newest first)", running)
	}
	if failed.Status != "FAILED" || failed.Error == nil || *failed.Error != "boom" {
		t.Errorf("runs[1] = %+v, want FAILED with error message 'boom'", failed)
	}
	if failed.WindowFrom != "2026-07-08" || failed.WindowTo != "2026-07-15" {
		t.Errorf("runs[1] window = %s–%s, want 2026-07-08–2026-07-15", failed.WindowFrom, failed.WindowTo)
	}
	if ok.Status != "OK" || ok.ItemsNew != 96 || ok.ItemsDeduped != 4 || ok.Error != nil {
		t.Errorf("runs[2] = %+v, want OK with 96 new / 4 deduped and no error", ok)
	}
	if ok.Source != acquisition.SourceDJEN {
		t.Errorf("runs[2].Source = %q, want %q (joined from integration)", ok.Source, acquisition.SourceDJEN)
	}
	if ok.FinishedAt == nil {
		t.Errorf("runs[2].FinishedAt = nil, want set on a closed run")
	}
}

// A tenant with no jobs, no runs and no acervo gets the empty view (import NONE,
// zero totals, zero runs) — not an error.
func TestAcquisition_Reconciliations_EmptyTenant(t *testing.T) {
	pool := newPool(t)
	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-recon-empty", 0)

	view, err := acquisition.NewReadUseCase(acquisition.NewRepository(pool)).
		Reconciliations(context.Background(), tenantID)
	if err != nil {
		t.Fatalf("Reconciliations: %v", err)
	}
	if view.Import.Importing || view.Import.Status != "NONE" {
		t.Fatalf("import = %+v, want NONE/not importing", view.Import)
	}
	if view.Totals.CourtRecords != 0 || view.Totals.Intimations != 0 || len(view.Runs) != 0 {
		t.Fatalf("view = %+v, want empty totals and runs", view)
	}
}

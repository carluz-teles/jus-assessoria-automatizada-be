//go:build integration

// Reconciliations read-model integration tests — prove the reconciliations reads
// against a REAL Postgres: the per-import umbrella (backfill_job aggregating its
// windows' new counts), the per-window detail (sync_run, chronological, error
// lifted from the jsonb), the collapse (a window's processes/intimations via
// sync_run_id), the acquired totals, the import banner state, and tenant isolation.
package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jusassessoria/platform/internal/acquisition"
)

// seedReconJob inserts an import (backfill_job) as the owner (RLS bypassed) and
// returns its id, so windows and the umbrella can be tied to it.
func seedReconJob(
	t *testing.T,
	pool *pgxpool.Pool,
	tenantID, integrationID, status, from, to string,
	total, ok, errCount int,
	createdAt time.Time,
) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO backfill_job
		   (tenant_id, integration_id, window_from, window_to, total_slices, slices_ok, slices_error, status, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id`,
		tenantID, integrationID, from, to, total, ok, errCount, status, createdAt).Scan(&id); err != nil {
		t.Fatalf("seed backfill_job: %v", err)
	}
	return id
}

// seedSyncRun inserts one window (sync_run) tied to a backfill_job as the owner,
// carrying the per-window NEW counts (processos/intimações), and returns its id.
// window/finishedAt empty → NULL; errMsg non-empty → the {"message": …} jsonb.
func seedSyncRun(
	t *testing.T,
	pool *pgxpool.Pool,
	tenantID, integrationID, backfillJobID, status, windowFrom, windowTo, errMsg string,
	courtRecordsNew, intimationsNew int,
	startedAt time.Time,
	finished bool,
) string {
	t.Helper()
	var window, windowEnd any
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
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO sync_run
		   (tenant_id, integration_id, backfill_job_id, connector_id, connector_version, started_at,
		    finished_at, status, court_records_new, intimations_new, error, window_from, window_to)
		 VALUES ($1, $2, $3, 'djen', 'v1', $4, $5, $6, $7, $8, $9, $10, $11) RETURNING id`,
		tenantID, integrationID, backfillJobID, startedAt, finishedAt, status,
		courtRecordsNew, intimationsNew, errJSON, window, windowEnd).Scan(&id); err != nil {
		t.Fatalf("seed sync_run: %v", err)
	}
	return id
}

// linkRecordToRun stamps the discovering window onto a court record (the collapse
// reads court_record.sync_run_id).
func linkRecordToRun(t *testing.T, pool *pgxpool.Pool, recordID, syncRunID string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE court_record SET sync_run_id = $2 WHERE id = $1`, recordID, syncRunID); err != nil {
		t.Fatalf("link record to run: %v", err)
	}
}

// seedIntimation inserts one intimation for the record, tied to its discovering
// window (sync_run_id) — enough to count in the totals and show in the collapse.
func seedIntimation(t *testing.T, pool *pgxpool.Pool, tenantID string, cr *acquisition.CourtRecord, syncRunID string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO intimation
		   (tenant_id, case_id, court_record_id, hash, made_available_at, published_at,
		    deadline_start_at, content, source, sync_run_id)
		 VALUES ($1, $2, $3, $4, '2026-01-05', '2026-01-06', '2026-01-07', 'teor', 'DJEN', $5)`,
		tenantID, cr.CaseID, cr.ID, uuid.NewString(), syncRunID); err != nil {
		t.Fatalf("seed intimation: %v", err)
	}
}

// The whole reconciliations read: import banner from the latest backfill_job,
// totals from the acervo, ONE umbrella aggregating its three windows, the detail's
// per-window rows (chronological, with the FAILED message), the collapse of the OK
// window — and nothing leaked from another tenant.
func TestAcquisition_Reconciliations_View(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-recon", 0)
	integrationID := seedIntegration(t, pool, tenantID, acquisition.SourceDJEN)

	otherTenant := uuid.NewString()
	seedTenant(t, pool, otherTenant, "org-recon-other", 0)
	otherIntegration := seedIntegration(t, pool, otherTenant, acquisition.SourceDJEN)

	// Import banner: one RUNNING backfill_job → importing=true.
	seedImportJob(t, pool, tenantID, integrationID, "RUNNING", 53, 19, 2, time.Now())

	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	// The umbrella under test: its own backfill_job with three windows (OK, FAILED,
	// RUNNING). The OK window discovered 5 processos / 8 intimações.
	jobID := seedReconJob(t, pool, tenantID, integrationID, "RUNNING", "2026-07-01", "2026-07-22", 53, 19, 2, base)
	okRun := seedSyncRun(t, pool, tenantID, integrationID, jobID, "OK", "2026-07-01", "2026-07-08", "", 5, 8, base, true)
	seedSyncRun(t, pool, tenantID, integrationID, jobID, "FAILED", "2026-07-08", "2026-07-15", "boom", 0, 0, base.Add(time.Hour), true)
	seedSyncRun(t, pool, tenantID, integrationID, jobID, "RUNNING", "2026-07-15", "2026-07-22", "", 0, 0, base.Add(2*time.Hour), false)

	// A court record + intimation the OK window discovered (totals + collapse).
	cr, err := findOrCreateCourtRecord(t, pool, tenantID, "0000001-11.2026.8.26.0001", 10)
	if err != nil {
		t.Fatalf("seed court record: %v", err)
	}
	linkRecordToRun(t, pool, cr.ID, okRun)
	seedIntimation(t, pool, tenantID, cr, okRun)

	// Another tenant's import must never surface.
	otherJob := seedReconJob(t, pool, otherTenant, otherIntegration, "COMPLETED", "2026-07-01", "2026-07-08", 1, 1, 0, base)
	seedSyncRun(t, pool, otherTenant, otherIntegration, otherJob, "OK", "2026-07-01", "2026-07-08", "", 999, 999, base, true)

	uc := acquisition.NewReadUseCase(acquisition.NewRepository(pool))

	view, err := uc.Reconciliations(ctx, tenantID)
	if err != nil {
		t.Fatalf("Reconciliations: %v", err)
	}

	if !view.Import.Importing || view.Import.Status != "RUNNING" {
		t.Fatalf("import = %+v, want importing RUNNING", view.Import)
	}
	if view.Totals.CourtRecords != 1 || view.Totals.Intimations != 1 {
		t.Fatalf("totals = %+v, want 1 processo / 1 intimação", view.Totals)
	}

	// One umbrella (the other tenant's must not leak), aggregating its windows.
	if len(view.Reconciliations) != 1 {
		t.Fatalf("reconciliations = %d, want 1 (another tenant's import must not leak)", len(view.Reconciliations))
	}
	u := view.Reconciliations[0]
	if u.ID != jobID {
		t.Errorf("umbrella id = %q, want the seeded job %q", u.ID, jobID)
	}
	if u.Processos != 5 || u.Intimacoes != 8 {
		t.Errorf("umbrella aggregates = %d proc / %d intim, want 5/8 (summed across windows)", u.Processos, u.Intimacoes)
	}
	if u.Status != "RUNNING" || u.TotalSlices != 53 || u.SlicesOK != 19 || u.SlicesError != 2 {
		t.Errorf("umbrella lifecycle = %+v, want RUNNING 19/2/53", u)
	}
	if u.FinishedAt != nil {
		t.Errorf("umbrella FinishedAt = %v, want nil while RUNNING", u.FinishedAt)
	}
	if u.WindowFrom != "2026-07-01" || u.WindowTo != "2026-07-22" {
		t.Errorf("umbrella window = %s–%s, want the job's overall window", u.WindowFrom, u.WindowTo)
	}

	// Detail: three windows, chronological (window_from ASC).
	detail, err := uc.ReconciliationDetail(ctx, tenantID, jobID)
	if err != nil {
		t.Fatalf("ReconciliationDetail: %v", err)
	}
	if detail.Reconciliation.ID != jobID {
		t.Errorf("detail umbrella id = %q, want %q", detail.Reconciliation.ID, jobID)
	}
	if len(detail.Windows) != 3 {
		t.Fatalf("windows = %d, want 3", len(detail.Windows))
	}
	w0, w1, w2 := detail.Windows[0], detail.Windows[1], detail.Windows[2]
	if w0.Status != "OK" || w0.ProcessosNew != 5 || w0.IntimacoesNew != 8 || w0.FinishedAt == nil {
		t.Errorf("windows[0] = %+v, want OK 5/8 finished (chronological first)", w0)
	}
	if w1.Status != "FAILED" || w1.Error == nil || *w1.Error != "boom" {
		t.Errorf("windows[1] = %+v, want FAILED with message 'boom'", w1)
	}
	if w2.Status != "RUNNING" || w2.FinishedAt != nil {
		t.Errorf("windows[2] = %+v, want an open RUNNING window", w2)
	}

	// Collapse: the OK window's discovered items.
	items, err := uc.SyncRunItems(ctx, tenantID, okRun)
	if err != nil {
		t.Fatalf("SyncRunItems: %v", err)
	}
	if len(items.Processos) != 1 || items.Processos[0].CNJNumber != "0000001-11.2026.8.26.0001" {
		t.Errorf("collapse processos = %+v, want the one linked record", items.Processos)
	}
	if len(items.Intimacoes) != 1 {
		t.Errorf("collapse intimações = %d, want 1", len(items.Intimacoes))
	}
}

// A tenant with no imports, no acervo gets the empty view (import NONE, zero
// totals, zero reconciliations) — not an error.
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
	if view.Totals.CourtRecords != 0 || view.Totals.Intimations != 0 || len(view.Reconciliations) != 0 {
		t.Fatalf("view = %+v, want empty totals and reconciliations", view)
	}
}

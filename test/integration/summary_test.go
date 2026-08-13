//go:build integration

// Summary read integration tests — prove the KPI aggregates (SummarizeProcessos /
// SummarizeIntimacoes) against a real Postgres: processes bucket by court_record
// lifecycle (SUPERSEDED excluded from total; baixados always 0 in v0), and intimações
// bucket by the triagem user_status (em_analise/criticas 0 for now). Both tenant-scoped.
package integration_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jusassessoria/platform/internal/acquisition"
)

// TestSummarizeProcessos_BucketsByLifecycle seeds one record per lifecycle value and
// asserts the buckets: em_andamento=ACTIVE, suspensos=SUSPENDED, arquivados=ARCHIVED,
// baixados=0 (no source), and total excludes the SUPERSEDED placeholder.
func TestSummarizeProcessos_BucketsByLifecycle(t *testing.T) {
	pool := newPool(t)
	repo := acquisition.NewRepository(pool)
	uc := acquisition.NewReadUseCase(repo)
	ctx := context.Background()

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-proc-summary", 0)
	seedRecordWithLifecycle(t, pool, tenantID, "0000001-00.2024.8.26.0001", acquisition.LifecycleActive)
	seedRecordWithLifecycle(t, pool, tenantID, "0000002-00.2024.8.26.0002", acquisition.LifecycleActive)
	seedRecordWithLifecycle(t, pool, tenantID, "0000003-00.2024.8.26.0003", "SUSPENDED")
	seedRecordWithLifecycle(t, pool, tenantID, "0000004-00.2024.8.26.0004", "ARCHIVED")
	seedRecordWithLifecycle(t, pool, tenantID, "0000005-00.2024.8.26.0005", acquisition.LifecycleSuperseded)

	got, err := uc.ProcessosSummary(ctx, tenantID)
	if err != nil {
		t.Fatalf("ProcessosSummary: %v", err)
	}
	want := acquisition.ProcessosSummaryView{
		Total: 4, EmAndamento: 2, Suspensos: 1, Arquivados: 1, Baixados: 0,
	}
	if got != want {
		t.Fatalf("summary = %+v, want %+v (SUPERSEDED excluded from total; baixados 0)", got, want)
	}
}

// TestSummarizeIntimacoes_BucketsByUserStatus seeds intimations across the triagem states
// and asserts the buckets: pendentes counts everything not resolved/ignored (PENDING),
// resolvidas=RESOLVED, ignoradas=IGNORED; em_analise and criticas are 0 (no source yet).
func TestSummarizeIntimacoes_BucketsByUserStatus(t *testing.T) {
	pool := newPool(t)
	repo := acquisition.NewRepository(pool)
	uc := acquisition.NewReadUseCase(repo)
	ctx := context.Background()

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-inti-summary", 0)
	rec, caseID := seedCourtRecordCNJ(t, pool, tenantID, "0000009-00.2024.8.26.0009")

	// 3 PENDING (default), 2 RESOLVED, 1 IGNORED.
	seedIntimationWithUserStatus(t, pool, tenantID, caseID, rec, acquisition.IntimationUserStatusPending)
	seedIntimationWithUserStatus(t, pool, tenantID, caseID, rec, acquisition.IntimationUserStatusPending)
	seedIntimationWithUserStatus(t, pool, tenantID, caseID, rec, acquisition.IntimationUserStatusPending)
	seedIntimationWithUserStatus(t, pool, tenantID, caseID, rec, acquisition.IntimationUserStatusResolved)
	seedIntimationWithUserStatus(t, pool, tenantID, caseID, rec, acquisition.IntimationUserStatusResolved)
	seedIntimationWithUserStatus(t, pool, tenantID, caseID, rec, acquisition.IntimationUserStatusIgnored)

	got, err := uc.IntimacoesSummary(ctx, tenantID)
	if err != nil {
		t.Fatalf("IntimacoesSummary: %v", err)
	}
	want := acquisition.IntimacoesSummaryView{
		Total: 6, Pendentes: 3, EmAnalise: 0, Resolvidas: 2, Ignoradas: 1, Criticas: 0,
	}
	if got != want {
		t.Fatalf("summary = %+v, want %+v", got, want)
	}
}

// seedRecordWithLifecycle inserts a court_record (with its case) at a given lifecycle,
// for the processes summary buckets.
func seedRecordWithLifecycle(t *testing.T, pool *pgxpool.Pool, tenantID, cnj, lifecycle string) {
	t.Helper()
	ctx := context.Background()
	var caseID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO court_case (tenant_id) VALUES ($1) RETURNING id::text`, tenantID).Scan(&caseID); err != nil {
		t.Fatalf("seed court_case: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO court_record (tenant_id, case_id, cnj_number, degree, court, completeness, lifecycle)
		 VALUES ($1, $2, $3, 'G1', 'TJSP', 0.5, $4)`,
		tenantID, caseID, cnj, lifecycle); err != nil {
		t.Fatalf("seed court_record: %v", err)
	}
}

// seedIntimationWithUserStatus inserts one intimation for the record at a given triagem
// user_status, for the intimações summary buckets.
func seedIntimationWithUserStatus(t *testing.T, pool *pgxpool.Pool, tenantID, caseID, recordID, userStatus string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO intimation
		   (tenant_id, case_id, court_record_id, hash, made_available_at, published_at,
		    deadline_start_at, content, source, user_status)
		 VALUES ($1, $2, $3, $4, '2026-01-05', '2026-01-06', '2026-01-07', 'teor', 'DJEN', $5)`,
		tenantID, caseID, recordID, uuid.NewString(), userStatus); err != nil {
		t.Fatalf("seed intimation: %v", err)
	}
}

//go:build integration

// Search + totals read integration tests — prove the ?search filter and the "X de Y"
// counters against a real Postgres (the ILIKE, the pg_trgm index, and the filtered vs
// global COUNTs from migration 0023 / the acquisition read model). Tenant-scoped.
package integration_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jusassessoria/platform/internal/acquisition"
)

// First-page keyset sentinels (mirror the handler's): the ascending processos scan
// starts below every row; the descending intimações scan above every row.
const (
	firstCNJ    = ""
	zeroUUIDlit = "00000000-0000-0000-0000-000000000000"
	maxDateLit  = "9999-12-31"
	maxUUIDlit  = "ffffffff-ffff-ffff-ffff-ffffffffffff"
)

// seedCourtRecordCNJ inserts one ACTIVE court_record (its own court_case) with the
// given cnj_number and returns (recordID, caseID). Owner insert (pool bypasses RLS).
func seedCourtRecordCNJ(t *testing.T, pool *pgxpool.Pool, tenantID, cnj string) (recordID, caseID string) {
	t.Helper()
	ctx := context.Background()
	if err := pool.QueryRow(ctx,
		`INSERT INTO court_case (tenant_id) VALUES ($1) RETURNING id::text`, tenantID).Scan(&caseID); err != nil {
		t.Fatalf("seed court_case: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO court_record (tenant_id, case_id, cnj_number, degree, court, completeness)
		 VALUES ($1, $2, $3, 'G1', 'TJSP', 0.5) RETURNING id::text`,
		tenantID, caseID, cnj).Scan(&recordID); err != nil {
		t.Fatalf("seed court_record: %v", err)
	}
	return recordID, caseID
}

// TestListProcessos_SearchFiltersAndTotals covers CA-BE-01/02/03: no search → all
// rows, total_count == total; a partial CNJ search → only matches, total_count is the
// filtered count while total stays the tenant-wide count.
func TestListProcessos_SearchFiltersAndTotals(t *testing.T) {
	pool := newPool(t)
	repo := acquisition.NewRepository(pool)
	uc := acquisition.NewReadUseCase(repo)
	ctx := context.Background()

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-proc-search", 0)
	seedCourtRecordCNJ(t, pool, tenantID, "0000123-45.2023.8.26.0001")
	seedCourtRecordCNJ(t, pool, tenantID, "0000123-99.2023.8.26.0002")
	seedCourtRecordCNJ(t, pool, tenantID, "5550000-00.2024.8.26.0500")

	// No search: all 3, and total_count == total.
	all, err := uc.Processos(ctx, acquisition.ProcessosQuery{
		TenantID: tenantID, Limit: 20, LastCNJ: firstCNJ, LastID: zeroUUIDlit,
	})
	if err != nil {
		t.Fatalf("Processos (no search): %v", err)
	}
	if len(all.Items) != 3 || all.Total != 3 || all.TotalCount != 3 {
		t.Fatalf("no search: items=%d total_count=%d total=%d, want 3/3/3", len(all.Items), all.TotalCount, all.Total)
	}

	// Partial CNJ "0000123" matches 2 of 3.
	got, err := uc.Processos(ctx, acquisition.ProcessosQuery{
		TenantID: tenantID, Limit: 20, LastCNJ: firstCNJ, LastID: zeroUUIDlit, Search: "0000123",
	})
	if err != nil {
		t.Fatalf("Processos (search): %v", err)
	}
	if len(got.Items) != 2 {
		t.Fatalf("search: %d rows, want 2", len(got.Items))
	}
	if got.TotalCount != 2 {
		t.Errorf("search total_count = %d, want 2 (filtered)", got.TotalCount)
	}
	if got.Total != 3 {
		t.Errorf("search total = %d, want 3 (global, unfiltered)", got.Total)
	}
	for _, p := range got.Items {
		if p.CNJNumber != "0000123-45.2023.8.26.0001" && p.CNJNumber != "0000123-99.2023.8.26.0002" {
			t.Errorf("unexpected row in filtered result: %s", p.CNJNumber)
		}
	}
}

// TestListProcessos_SearchComposesWithCursor covers CA-BE-05: search + keyset cursor
// page through the filtered set with no overlap and no gap.
func TestListProcessos_SearchComposesWithCursor(t *testing.T) {
	pool := newPool(t)
	repo := acquisition.NewRepository(pool)
	uc := acquisition.NewReadUseCase(repo)
	ctx := context.Background()

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-proc-cursor", 0)
	// Three matches (share "777") plus one non-match, to prove the filter holds across pages.
	seedCourtRecordCNJ(t, pool, tenantID, "0007771-11.2023.8.26.0001")
	seedCourtRecordCNJ(t, pool, tenantID, "0007772-22.2023.8.26.0002")
	seedCourtRecordCNJ(t, pool, tenantID, "0007773-33.2023.8.26.0003")
	seedCourtRecordCNJ(t, pool, tenantID, "9990000-00.2024.8.26.0500")

	// Page 1: limit 2 over the filtered set → 2 rows, more remain.
	p1, err := uc.Processos(ctx, acquisition.ProcessosQuery{
		TenantID: tenantID, Limit: 2, LastCNJ: firstCNJ, LastID: zeroUUIDlit, Search: "777",
	})
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if len(p1.Items) != 2 || !p1.HasMore {
		t.Fatalf("page 1: items=%d hasMore=%v, want 2/true", len(p1.Items), p1.HasMore)
	}
	if p1.TotalCount != 3 {
		t.Errorf("page 1 total_count = %d, want 3", p1.TotalCount)
	}

	// Page 2: resume from page 1's last row's keyset.
	last := p1.Items[len(p1.Items)-1]
	p2, err := uc.Processos(ctx, acquisition.ProcessosQuery{
		TenantID: tenantID, Limit: 2, LastCNJ: last.CNJNumber, LastID: last.ID, Search: "777",
	})
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if len(p2.Items) != 1 || p2.HasMore {
		t.Fatalf("page 2: items=%d hasMore=%v, want 1/false", len(p2.Items), p2.HasMore)
	}
	// No overlap between the pages.
	seen := map[string]bool{p1.Items[0].ID: true, p1.Items[1].ID: true}
	if seen[p2.Items[0].ID] {
		t.Errorf("page 2 row %s overlaps page 1", p2.Items[0].ID)
	}
}

// TestListIntimacoes_SearchByCourtRecordCNJ covers CA-BE-04: intimations filter by the
// JOINed court record's cnj_number (not the intimation content).
func TestListIntimacoes_SearchByCourtRecordCNJ(t *testing.T) {
	pool := newPool(t)
	repo := acquisition.NewRepository(pool)
	uc := acquisition.NewReadUseCase(repo)
	ctx := context.Background()

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-inti-search", 0)

	recA, caseA := seedCourtRecordCNJ(t, pool, tenantID, "0004567-11.2023.8.26.0001")
	recB, caseB := seedCourtRecordCNJ(t, pool, tenantID, "8880000-22.2024.8.26.0500")
	seedIntimationFor(t, pool, tenantID, caseA, recA)
	seedIntimationFor(t, pool, tenantID, caseB, recB)

	got, err := uc.Intimacoes(ctx, acquisition.IntimacoesQuery{
		TenantID: tenantID, Limit: 20, LastMadeAvailable: maxDateLit, LastID: maxUUIDlit, Search: "0004567",
	})
	if err != nil {
		t.Fatalf("Intimacoes (search): %v", err)
	}
	if len(got.Items) != 1 {
		t.Fatalf("search: %d rows, want 1", len(got.Items))
	}
	if got.Items[0].CNJNumber != "0004567-11.2023.8.26.0001" {
		t.Errorf("matched wrong intimation: %s", got.Items[0].CNJNumber)
	}
	if got.TotalCount != 1 || got.Total != 2 {
		t.Errorf("totals = (%d, %d), want (1, 2)", got.TotalCount, got.Total)
	}
}

// seedIntimationFor inserts one intimation for the record (no discovering window —
// sync_run_id stays NULL), enough for the inbox read and its counts.
func seedIntimationFor(t *testing.T, pool *pgxpool.Pool, tenantID, caseID, recordID string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO intimation
		   (tenant_id, case_id, court_record_id, hash, made_available_at, published_at,
		    deadline_start_at, content, source)
		 VALUES ($1, $2, $3, $4, '2026-01-05', '2026-01-06', '2026-01-07', 'teor', 'DJEN')`,
		tenantID, caseID, recordID, uuid.NewString()); err != nil {
		t.Fatalf("seed intimation: %v", err)
	}
}

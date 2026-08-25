//go:build integration

// Bucket-boundary and ?assignee integration tests for the intimações inbox
// (internal/acquisition ListIntimacoes/CountIntimacoesBuckets), against a real
// Postgres — the day-boundary arithmetic and the "sem providência"/assignee
// predicates live in SQL, so they are proved here rather than with a mocked repo.
package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jusassessoria/platform/internal/acquisition"
)

// seedAppUserReturningID inserts one app_user for the tenant and returns its id, for
// tests that need a real user id to assign as condutor/revisor.
func seedAppUserReturningID(t *testing.T, pool *pgxpool.Pool, tenantID, clerkUserID, email string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO app_user (clerk_user_id, tenant_id, email) VALUES ($1, $2, $3) RETURNING id::text`,
		clerkUserID, tenantID, email,
	).Scan(&id); err != nil {
		t.Fatalf("seed app_user: %v", err)
	}
	return id
}

// seedDeadlineFor inserts an OPEN deadline for the given intimation, end_date days
// from today (may be negative), so days_left arithmetic in the bucket queries is
// exercised against the real CURRENT_DATE.
func seedDeadlineFor(t *testing.T, pool *pgxpool.Pool, tenantID, recordID, intimationID string, daysFromNow int) {
	t.Helper()
	end := time.Now().UTC().AddDate(0, 0, daysFromNow).Format(time.DateOnly)
	mustExec(t, pool,
		`INSERT INTO deadline
		   (tenant_id, court_record_id, notification_id, start_date, end_date, days, counting, status)
		 VALUES ($1, $2, $3, CURRENT_DATE, $4::date, 5, 'BUSINESS', 'OPEN')`,
		tenantID, recordID, intimationID, end)
}

// TestListIntimacoes_UrgenciaBucketBoundaries covers the redefined day-left ranges:
// days_left = 2 falls in proximos_dois_dias, days_left = 3 falls in esta_semana — the
// boundary the architect moved (old range was a single 1-7 "semana" bucket).
func TestListIntimacoes_UrgenciaBucketBoundaries(t *testing.T) {
	pool := newPool(t)
	repo := acquisition.NewRepository(pool)
	uc := acquisition.NewReadUseCase(repo)
	ctx := context.Background()

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-bucket-boundary", 0)
	recA, caseA := seedCourtRecordCNJ(t, pool, tenantID, "0001000-11.2026.8.26.0001")
	recB, caseB := seedCourtRecordCNJ(t, pool, tenantID, "0002000-22.2026.8.26.0002")

	twoDays := seedIntimationReturningID(t, pool, tenantID, caseA, recA)
	seedDeadlineFor(t, pool, tenantID, recA, twoDays, 2)
	threeDays := seedIntimationReturningID(t, pool, tenantID, caseB, recB)
	seedDeadlineFor(t, pool, tenantID, recB, threeDays, 3)

	gotProximos, err := uc.Intimacoes(ctx, acquisition.IntimacoesQuery{
		TenantID: tenantID, Limit: 20, LastMadeAvailable: maxDateLit, LastID: maxUUIDlit,
		Urgencia: acquisition.UrgenciaProximosDoisDias,
	})
	if err != nil {
		t.Fatalf("Intimacoes (proximos_dois_dias): %v", err)
	}
	if len(gotProximos.Items) != 1 || gotProximos.Items[0].ID != twoDays {
		t.Fatalf("proximos_dois_dias: got %d items, want exactly the days_left=2 row", len(gotProximos.Items))
	}
	if gotProximos.Buckets.ProximosDoisDias != 1 {
		t.Errorf("Buckets.ProximosDoisDias = %d, want 1", gotProximos.Buckets.ProximosDoisDias)
	}
	if gotProximos.Buckets.EstaSemana != 1 {
		t.Errorf("Buckets.EstaSemana = %d, want 1 (the days_left=3 row)", gotProximos.Buckets.EstaSemana)
	}

	gotSemana, err := uc.Intimacoes(ctx, acquisition.IntimacoesQuery{
		TenantID: tenantID, Limit: 20, LastMadeAvailable: maxDateLit, LastID: maxUUIDlit,
		Urgencia: acquisition.UrgenciaSemana,
	})
	if err != nil {
		t.Fatalf("Intimacoes (semana): %v", err)
	}
	if len(gotSemana.Items) != 1 || gotSemana.Items[0].ID != threeDays {
		t.Fatalf("semana: got %d items, want exactly the days_left=3 row", len(gotSemana.Items))
	}
}

// TestListIntimacoes_AssigneeMe_MatchesConductorOrReviewer covers the "Minhas" toggle:
// ?assignee resolves to a user id that matches EITHER the condutor OR the revisor —
// the Architect's OR-between-roles decision.
func TestListIntimacoes_AssigneeMe_MatchesConductorOrReviewer(t *testing.T) {
	pool := newPool(t)
	repo := acquisition.NewRepository(pool)
	uc := acquisition.NewReadUseCase(repo)
	ctx := context.Background()

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-assignee-me", 0)

	// The SAME target user (underTest) is the condutor of one intimação and the revisor
	// of another — proving the OR between the two roles, not just a single-column match.
	underTest := seedAppUserReturningID(t, pool, tenantID, "org-assignee-me-under-test", "under-test@org-assignee-me.test")
	otherID := seedAppUserReturningID(t, pool, tenantID, "org-assignee-me-other", "other@org-assignee-me.test")

	recA, caseA := seedCourtRecordCNJ(t, pool, tenantID, "0005000-11.2026.8.26.0001")
	recB, caseB := seedCourtRecordCNJ(t, pool, tenantID, "0006000-22.2026.8.26.0002")
	recC, caseC := seedCourtRecordCNJ(t, pool, tenantID, "0007000-33.2026.8.26.0003")

	asConductor := seedIntimationReturningID(t, pool, tenantID, caseA, recA)
	mustExec(t, pool, `UPDATE intimation SET conductor_user_id = $1 WHERE id = $2`, underTest, asConductor)
	asReviewer := seedIntimationReturningID(t, pool, tenantID, caseB, recB)
	mustExec(t, pool, `UPDATE intimation SET reviewer_user_id = $1 WHERE id = $2`, underTest, asReviewer)
	unrelated := seedIntimationReturningID(t, pool, tenantID, caseC, recC)
	mustExec(t, pool, `UPDATE intimation SET conductor_user_id = $1 WHERE id = $2`, otherID, unrelated)

	got, err := uc.Intimacoes(ctx, acquisition.IntimacoesQuery{
		TenantID: tenantID, Limit: 20, LastMadeAvailable: maxDateLit, LastID: maxUUIDlit,
		Assignee: underTest,
	})
	if err != nil {
		t.Fatalf("Intimacoes (assignee): %v", err)
	}
	gotIDs := map[string]bool{}
	for _, item := range got.Items {
		gotIDs[item.ID] = true
	}
	if len(got.Items) != 2 || !gotIDs[asConductor] || !gotIDs[asReviewer] {
		t.Fatalf("assignee=underTest: got %v, want exactly [%s, %s] (condutor OR revisor)", gotIDs, asConductor, asReviewer)
	}
	if gotIDs[unrelated] {
		t.Errorf("assignee=underTest unexpectedly matched an unrelated intimação %s", unrelated)
	}
}

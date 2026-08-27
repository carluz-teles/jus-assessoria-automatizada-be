//go:build integration

// Onboarding slice integration test — proves the GetProgress EXISTS-subquery
// SQL against a REAL Postgres reading OTHER slices' tables (integration,
// membership, intimation, process_activity_log), and that Dismiss's upsert is
// tenant-scoped, per-user, and idempotent. Unit tests (internal/onboarding)
// already cover the use case with a mocked repo; this proves the actual SQL.
package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jusassessoria/platform/internal/onboarding"
	"github.com/jusassessoria/platform/lib/database"
)

// seedIntimationAnalyzed inserts an intimation row with ai_analyzed_at stamped
// — the first_analise signal GetProgress reads. Mirrors
// seedIntimationWithUserStatus (summary_test.go) but for the AI-analysis
// column instead of user_status.
func seedIntimationAnalyzed(t *testing.T, pool *pgxpool.Pool, tenantID, caseID, recordID string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO intimation
		   (tenant_id, case_id, court_record_id, hash, made_available_at, published_at,
		    deadline_start_at, content, source, ai_analyzed_at)
		 VALUES ($1, $2, $3, $4, '2026-01-05', '2026-01-06', '2026-01-07', 'teor', 'DJEN', now())`,
		tenantID, caseID, recordID, uuid.NewString()); err != nil {
		t.Fatalf("seed analyzed intimation: %v", err)
	}
}

// TestOnboarding_GetProgress_TracksRealActivation drives every one of the 5
// steps to true via the SAME tables the real product writes (integration,
// membership, intimation, process_activity_log), then asserts GetProgress
// reads all of them back through the actual EXISTS SQL. Also proves
// members_invited excludes the caller itself.
func TestOnboarding_GetProgress_TracksRealActivation(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	repo := onboarding.NewRepository(pool)

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-onboarding-progress", 0)
	adminID := seedAppUserReturningID(t, pool, tenantID, "org-onboarding-progress-admin", "admin@onboarding.test")

	// Nothing activated yet: every step false, never dismissed.
	got, err := repo.GetProgress(ctx, tenantID, adminID)
	if err != nil {
		t.Fatalf("GetProgress (baseline): %v", err)
	}
	if got != (onboarding.Progress{}) {
		t.Fatalf("baseline GetProgress = %+v, want the zero value", got)
	}

	// sources_connected: an integration row for the tenant.
	seedIntegration(t, pool, tenantID, "DJEN")

	// members_invited: an ACTIVE membership for the caller ITSELF must NOT flip
	// this true — a lone admin is not "team invited".
	mustExec(t, pool,
		`INSERT INTO membership (tenant_id, app_user_id, role, status) VALUES ($1, $2, 'ADMIN', 'ACTIVE')`,
		tenantID, adminID)
	got, err = repo.GetProgress(ctx, tenantID, adminID)
	if err != nil {
		t.Fatalf("GetProgress (self membership only): %v", err)
	}
	if got.Steps.MembersInvited {
		t.Fatal("MembersInvited = true after only the caller's own membership, want false")
	}
	if !got.Steps.SourcesConnected {
		t.Fatal("SourcesConnected = false after seeding an integration row, want true")
	}

	// A SECOND, different member flips members_invited true.
	colleagueID := seedAppUserReturningID(t, pool, tenantID, "org-onboarding-progress-colleague", "colleague@onboarding.test")
	mustExec(t, pool,
		`INSERT INTO membership (tenant_id, app_user_id, role, status) VALUES ($1, $2, 'LAWYER', 'ACTIVE')`,
		tenantID, colleagueID)

	// first_triagem / first_analise / first_peca: one court_record backs both a
	// RESOLVED intimation and an analyzed one, plus a DRAFT_GENERATED activity row.
	recordID, caseID := seedCourtRecordCNJ(t, pool, tenantID, "0000001-11.2026.8.26.0100")
	seedIntimationWithUserStatus(t, pool, tenantID, caseID, recordID, "RESOLVED")
	seedIntimationAnalyzed(t, pool, tenantID, caseID, recordID)
	seedActivityLogRow(t, pool, tenantID, recordID, "DRAFT_GENERATED")

	got, err = repo.GetProgress(ctx, tenantID, adminID)
	if err != nil {
		t.Fatalf("GetProgress (fully activated): %v", err)
	}
	want := onboarding.Progress{Steps: onboarding.Steps{
		SourcesConnected: true,
		MembersInvited:   true,
		FirstTriagem:     true,
		FirstAnalise:     true,
		FirstPeca:        true,
	}}
	if got != want {
		t.Fatalf("GetProgress (fully activated) = %+v, want %+v", got, want)
	}
}

// TestOnboarding_GetProgress_TenantIsolation proves barrier 2 (RLS) plus the
// WHERE tenant_id = $1 barrier 1: tenant B's activation never leaks into
// tenant A's read, even though both tenants have real rows in the SAME
// tables.
func TestOnboarding_GetProgress_TenantIsolation(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	repo := onboarding.NewRepository(pool)

	tenantA := uuid.NewString()
	tenantB := uuid.NewString()
	seedTenant(t, pool, tenantA, "org-onboarding-iso-a", 0)
	seedTenant(t, pool, tenantB, "org-onboarding-iso-b", 0)
	userA := seedAppUserReturningID(t, pool, tenantA, "org-onboarding-iso-a-admin", "a@onboarding-iso.test")
	userB := seedAppUserReturningID(t, pool, tenantB, "org-onboarding-iso-b-admin", "b@onboarding-iso.test")

	// Only tenant B activates a source.
	seedIntegration(t, pool, tenantB, "DJEN")

	gotA, err := repo.GetProgress(ctx, tenantA, userA)
	if err != nil {
		t.Fatalf("GetProgress(tenantA): %v", err)
	}
	if gotA.Steps.SourcesConnected {
		t.Fatal("tenant A sees tenant B's integration — isolation broken")
	}

	gotB, err := repo.GetProgress(ctx, tenantB, userB)
	if err != nil {
		t.Fatalf("GetProgress(tenantB): %v", err)
	}
	if !gotB.Steps.SourcesConnected {
		t.Fatal("tenant B does not see its own integration")
	}
}

// TestOnboarding_Dismiss_IdempotentAndPerUser drives Dismiss through the real
// UnitOfWork (the same transactional path the handler uses): a repeat dismiss
// never errors and just restamps dismissed_at, and each app_user's dismissal
// is independent — a colleague's own progress read stays undismissed.
func TestOnboarding_Dismiss_IdempotentAndPerUser(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	uow := database.NewUnitOfWork(pool)
	uc := onboarding.NewUseCase(onboarding.NewRepository(pool), uow)

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-onboarding-dismiss", 0)
	adminID := seedAppUserReturningID(t, pool, tenantID, "org-onboarding-dismiss-admin", "admin@onboarding-dismiss.test")
	colleagueID := seedAppUserReturningID(t, pool, tenantID, "org-onboarding-dismiss-colleague", "colleague@onboarding-dismiss.test")

	before, err := uc.GetProgress(ctx, tenantID, adminID)
	if err != nil {
		t.Fatalf("GetProgress (before dismiss): %v", err)
	}
	if before.DismissedAt != nil {
		t.Fatalf("DismissedAt = %v before any dismiss, want nil", before.DismissedAt)
	}

	if err := uc.Dismiss(ctx, tenantID, adminID); err != nil {
		t.Fatalf("Dismiss (first call): %v", err)
	}
	afterFirst, err := uc.GetProgress(ctx, tenantID, adminID)
	if err != nil {
		t.Fatalf("GetProgress (after first dismiss): %v", err)
	}
	if afterFirst.DismissedAt == nil {
		t.Fatal("DismissedAt is nil after Dismiss, want set")
	}
	firstStamp := *afterFirst.DismissedAt

	// A repeat dismiss must not error (upsert, ON CONFLICT DO UPDATE) and
	// restamps dismissed_at to a later (or equal, clock-resolution permitting)
	// time — never breaks, never duplicates the row (app_user_id is the PK).
	time.Sleep(5 * time.Millisecond)
	if err := uc.Dismiss(ctx, tenantID, adminID); err != nil {
		t.Fatalf("Dismiss (second call): %v, want nil (idempotent)", err)
	}
	afterSecond, err := uc.GetProgress(ctx, tenantID, adminID)
	if err != nil {
		t.Fatalf("GetProgress (after second dismiss): %v", err)
	}
	if afterSecond.DismissedAt == nil {
		t.Fatal("DismissedAt is nil after the second dismiss, want set")
	}
	if !afterSecond.DismissedAt.After(firstStamp) {
		t.Fatalf("second dismiss stamp %v did not advance past the first %v", *afterSecond.DismissedAt, firstStamp)
	}

	// The colleague never dismissed — their OWN read stays undismissed even
	// though they share the tenant with the admin who did.
	colleagueProgress, err := uc.GetProgress(ctx, tenantID, colleagueID)
	if err != nil {
		t.Fatalf("GetProgress (colleague): %v", err)
	}
	if colleagueProgress.DismissedAt != nil {
		t.Fatalf("colleague DismissedAt = %v, want nil (dismissal is per-user)", colleagueProgress.DismissedAt)
	}
}

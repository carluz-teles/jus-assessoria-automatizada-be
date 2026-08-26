//go:build integration

// Cascata de responsável — integration tests against a REAL Postgres.
//
// R1 (acquisition): AssignResponsible cascades the case's responsável onto every
// intimação anchored under it (via court_record_id → court_record.case_id), and ONLY
// those — a second, unrelated processo's intimações must stay untouched.
//
// R2 (deadline): CreateTask snapshots the intimação's responsável onto a new task at
// CREATE time — it is NOT a continuous link, so a later change to the intimação's
// assignee must not retroactively change the task.
package integration_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jusassessoria/platform/internal/deadline"
)

// seedIntimationForReturningID inserts one intimação for the given (case, court_record),
// owner insert (RLS bypassed), and returns its id.
func seedIntimationForReturningID(t *testing.T, pool *pgxpool.Pool, tenantID, caseID, recordID string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO intimation
		   (tenant_id, case_id, court_record_id, hash, made_available_at, published_at,
		    deadline_start_at, content, source)
		 VALUES ($1, $2, $3, $4, '2026-01-05', '2026-01-06', '2026-01-07', 'teor', 'DJEN')
		 RETURNING id::text`,
		tenantID, caseID, recordID, uuid.NewString()).Scan(&id); err != nil {
		t.Fatalf("seed intimation: %v", err)
	}
	return id
}

// intimationAssignee reads back one intimação's assignee_user_id (NULL → "").
func intimationAssignee(t *testing.T, pool *pgxpool.Pool, intimationID string) string {
	t.Helper()
	var assignee *string
	if err := pool.QueryRow(context.Background(),
		`SELECT assignee_user_id::text FROM intimation WHERE id = $1`, intimationID).Scan(&assignee); err != nil {
		t.Fatalf("read intimation assignee: %v", err)
	}
	if assignee == nil {
		return ""
	}
	return *assignee
}

// TestAssignResponsible_CascadesOnlyToOwnProcessIntimations proves R1 end to end: two
// processos (each its own case/record) with one intimação each. Assigning a responsável
// on process A's record cascades to A's intimação only — process B's stays untouched.
func TestAssignResponsible_CascadesOnlyToOwnProcessIntimations(t *testing.T) {
	pool := newPool(t)
	uc := newAcquisitionUC(pool)
	ctx := context.Background()

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-cascade-r1", 0)
	userID := seedAppUserReturningID(t, pool, tenantID, "clerk-cascade-r1", "cascade-r1@test.example")

	recordA, caseA := seedCourtRecordCNJ(t, pool, tenantID, "0000111-11.2023.8.26.0001")
	recordB, caseB := seedCourtRecordCNJ(t, pool, tenantID, "0000222-22.2023.8.26.0002")
	intimA := seedIntimationForReturningID(t, pool, tenantID, caseA, recordA)
	intimB := seedIntimationForReturningID(t, pool, tenantID, caseB, recordB)

	if err := uc.AssignResponsible(ctx, tenantID, recordA, &userID); err != nil {
		t.Fatalf("AssignResponsible: %v", err)
	}

	if got := intimationAssignee(t, pool, intimA); got != userID {
		t.Errorf("process A intimação assignee = %q, want %q (cascaded)", got, userID)
	}
	if got := intimationAssignee(t, pool, intimB); got != "" {
		t.Errorf("process B intimação assignee = %q, want empty (untouched by A's cascade)", got)
	}
}

// TestCreateTask_InheritedAssigneeIsSnapshotNotLiveLink proves R2: a task created with
// intimation_id set (and no explicit assignee) snapshots the intimação's assignee at
// CREATE time. Changing the intimação's assignee afterward must NOT retroactively change
// the already-created task — it is a one-time inheritance, not a continuous link.
func TestCreateTask_InheritedAssigneeIsSnapshotNotLiveLink(t *testing.T) {
	pool := newPool(t)
	acqUC := newAcquisitionUC(pool)
	dlUC := newDeadlineUC(pool)
	ctx := context.Background()

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-cascade-r2", 0)
	userX := seedAppUserReturningID(t, pool, tenantID, "clerk-cascade-r2-x", "cascade-r2-x@test.example")
	userY := seedAppUserReturningID(t, pool, tenantID, "clerk-cascade-r2-y", "cascade-r2-y@test.example")

	record, caseID := seedCourtRecordCNJ(t, pool, tenantID, "0000333-33.2023.8.26.0003")
	intimID := seedIntimationForReturningID(t, pool, tenantID, caseID, record)

	// Assign X to the process → cascades onto the intimação.
	if err := acqUC.AssignResponsible(ctx, tenantID, record, &userX); err != nil {
		t.Fatalf("AssignResponsible: %v", err)
	}
	if got := intimationAssignee(t, pool, intimID); got != userX {
		t.Fatalf("precondition: intimação assignee = %q, want %q", got, userX)
	}

	// Create a task inheriting from the intimação (no explicit assignee).
	task, err := dlUC.CreateTask(ctx, deadline.CreateTaskCommand{
		TenantID: tenantID, UserID: userX, IntimationID: intimID,
		Title: "Peça de resposta",
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if task.AssigneeUserID != userX {
		t.Fatalf("task assignee at creation = %q, want inherited %q", task.AssigneeUserID, userX)
	}

	// Now reassign the process (and thus the intimação, via cascade) to Y.
	if err := acqUC.AssignResponsible(ctx, tenantID, record, &userY); err != nil {
		t.Fatalf("AssignResponsible (reassign): %v", err)
	}
	if got := intimationAssignee(t, pool, intimID); got != userY {
		t.Fatalf("intimação assignee after reassign = %q, want %q", got, userY)
	}

	// The already-created task must still read X — a snapshot, not a live link.
	var taskAssignee *string
	if err := pool.QueryRow(ctx,
		`SELECT assignee_user_id::text FROM task WHERE id = $1`, task.ID).Scan(&taskAssignee); err != nil {
		t.Fatalf("read task assignee: %v", err)
	}
	if taskAssignee == nil || *taskAssignee != userX {
		t.Fatalf("task assignee after intimação reassign = %v, want unchanged %q (no live link)", taskAssignee, userX)
	}
}

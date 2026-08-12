//go:build integration

// Deadline confirm integration tests — prove the F2 "Aprovar tudo" write path (slice 5a,
// docs/erd-prazos.md §9) end to end against a REAL Postgres: seed a PENDING prazo via the
// creation path, then Confirm it and assert — in ONE tx — the prazo flips to OPEN with the
// RECOMPUTED end_date (through the real judicial calendar) + confirmed_by/at, the N tasks
// land in the task table, and a deadline.updated + one task.created per task commit to the
// outbox. These drive the real use case (real repo + calendar + outbox + uow), the same
// composition cmd/api mounts for the confirm route.
package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jusassessoria/platform/internal/deadline"
)

// mustDate parses a wire date (2006-01-02) for a task due_date, failing the test on a
// malformed literal.
func mustDate(t *testing.T, s string) time.Time {
	t.Helper()
	d, err := time.Parse(time.DateOnly, s)
	if err != nil {
		t.Fatalf("parse date %q: %v", s, err)
	}
	return d
}

// DLC1: seed intimation.observed → PENDING prazo (INTIMACAO on TJSP → the seeded
// MANIFESTACAO/5/BUSINESS rule, end 2024-03-11), then Confirm with a DIFFERENT day count
// (10 BUSINESS) + two tasks. The prazo must flip to OPEN with a RECOMPUTED end (2024-03-18,
// not the PENDING 2024-03-11), stamped confirmed_by/at, and the two tasks + the
// deadline.updated + two task.created must all be committed.
func TestDeadline_Confirm_FlipsOpenRecomputesAndWritesTasks(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)
	p := seedDeadlineParentsCommitted(ctx, t, pool)
	uc := newDeadlineUC(pool)

	// Seed the PENDING prazo (the F2 suggestion) via the creation path.
	obs := observedFor(p, uuid.NewString(), "INTIMACAO", "TJSP", "SP", "2024-03-04")
	if err := uc.OnIntimationObserved(ctx, obs); err != nil {
		t.Fatalf("OnIntimationObserved() error = %v", err)
	}

	var deadlineID, pendingEnd string
	if err := pool.QueryRow(ctx,
		`SELECT id, end_date::text FROM deadline WHERE notification_id = $1`, p.intimationID).
		Scan(&deadlineID, &pendingEnd); err != nil {
		t.Fatalf("read pending deadline: %v", err)
	}
	if pendingEnd != "2024-03-11" {
		t.Fatalf("pending end_date = %q, want 2024-03-11 (5 business days)", pendingEnd)
	}

	// The human confirms: CONTESTACAO, 10 dias úteis, one dated+assigned task and one bare.
	userID := uuid.NewString()
	assignee := uuid.NewString()
	due := mustDate(t, "2024-03-15")
	cmd := deadline.ConfirmCommand{
		TenantID:     p.tenantID.String(),
		UserID:       userID,
		IntimationID: p.intimationID.String(),
		Kind:         deadline.KindContestacao,
		Days:         10,
		Counting:     deadline.CountingBusiness,
		Tasks: []deadline.ConfirmTaskInput{
			{Title: "Protocolar contestação", Kind: "PECA", DueDate: &due, AssigneeUserID: assignee},
			{Title: "Dar ciência ao cliente"},
		},
	}
	res, err := uc.Confirm(ctx, cmd)
	if err != nil {
		t.Fatalf("Confirm() error = %v", err)
	}
	if res.Deadline.ID != deadlineID || len(res.Tasks) != 2 {
		t.Errorf("result deadline/tasks = %q/%d, want %q/2", res.Deadline.ID, len(res.Tasks), deadlineID)
	}

	// The prazo flipped to OPEN with the recomputed end + the approved fields + the stamp.
	var (
		status, kind, counting, endDate string
		days                            int
		confirmedBy                     *string
		confirmedAt                     *string
	)
	if err := pool.QueryRow(ctx, `
		SELECT status, kind, days, counting, end_date::text, confirmed_by::text, confirmed_at::text
		FROM deadline WHERE id = $1`, deadlineID).
		Scan(&status, &kind, &days, &counting, &endDate, &confirmedBy, &confirmedAt); err != nil {
		t.Fatalf("read confirmed deadline: %v", err)
	}
	if status != "OPEN" {
		t.Errorf("status = %q, want OPEN", status)
	}
	if kind != "CONTESTACAO" || days != 10 || counting != "BUSINESS" {
		t.Errorf("kind/days/counting = %q/%d/%q, want CONTESTACAO/10/BUSINESS", kind, days, counting)
	}
	if endDate != "2024-03-18" {
		t.Errorf("end_date = %q, want 2024-03-18 (10 business days, recomputed)", endDate)
	}
	if confirmedBy == nil || *confirmedBy != userID {
		t.Errorf("confirmed_by = %v, want %q", confirmedBy, userID)
	}
	if confirmedAt == nil {
		t.Error("confirmed_at = NULL, want a timestamp")
	}

	// The two tasks landed with the confirm context (OPEN, MANUAL, created_by, FKs).
	var taskCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM task
		WHERE deadline_id = $1 AND tenant_id = $2 AND intimation_id = $3
		  AND court_record_id = $4 AND status = 'OPEN' AND source = 'MANUAL' AND created_by = $5`,
		deadlineID, p.tenantID, p.intimationID, p.courtRecordID, userID).Scan(&taskCount); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if taskCount != 2 {
		t.Errorf("task rows = %d, want 2 (OPEN/MANUAL/created_by/FKs)", taskCount)
	}

	// The dated+assigned task carries its due_date + assignee; the bare one has neither.
	var datedAssigned int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM task
		WHERE deadline_id = $1 AND due_date = '2024-03-15' AND assignee_user_id = $2 AND title = 'Protocolar contestação'`,
		deadlineID, assignee).Scan(&datedAssigned); err != nil {
		t.Fatalf("count dated task: %v", err)
	}
	if datedAssigned != 1 {
		t.Errorf("dated+assigned task rows = %d, want 1", datedAssigned)
	}
	var bare int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM task
		WHERE deadline_id = $1 AND title = 'Dar ciência ao cliente' AND due_date IS NULL AND assignee_user_id IS NULL`,
		deadlineID).Scan(&bare); err != nil {
		t.Fatalf("count bare task: %v", err)
	}
	if bare != 1 {
		t.Errorf("bare task rows = %d, want 1 (no due_date, no assignee)", bare)
	}

	// Exactly one deadline.updated (aggregate = deadline id), payload OPEN + recomputed end.
	var updatedCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM outbox
		WHERE type = $1 AND aggregate_type = 'deadline' AND aggregate_id = $2`,
		deadline.TypeDeadlineUpdated, deadlineID).Scan(&updatedCount); err != nil {
		t.Fatalf("count deadline.updated: %v", err)
	}
	if updatedCount != 1 {
		t.Fatalf("deadline.updated rows = %d, want 1", updatedCount)
	}
	var updEnd, updStatus, updCounting string
	if err := pool.QueryRow(ctx, `
		SELECT payload->>'end_date', payload->>'status', payload->>'counting'
		FROM outbox WHERE type = $1 AND aggregate_id = $2`,
		deadline.TypeDeadlineUpdated, deadlineID).Scan(&updEnd, &updStatus, &updCounting); err != nil {
		t.Fatalf("read deadline.updated payload: %v", err)
	}
	if updEnd != "2024-03-18" || updStatus != "OPEN" || updCounting != "BUSINESS" {
		t.Errorf("deadline.updated end/status/counting = %q/%q/%q, want 2024-03-18/OPEN/BUSINESS", updEnd, updStatus, updCounting)
	}

	// Two task.created (aggregate_type = task), both pointing at the confirmed prazo.
	var taskCreatedCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM outbox
		WHERE type = $1 AND aggregate_type = 'task' AND payload->>'deadline_id' = $2`,
		deadline.TypeTaskCreated, deadlineID).Scan(&taskCreatedCount); err != nil {
		t.Fatalf("count task.created: %v", err)
	}
	if taskCreatedCount != 2 {
		t.Errorf("task.created rows = %d, want 2", taskCreatedCount)
	}
}

// DLC2: confirming an intimação with NO derived prazo is the typed ErrDeadlineNotFound (→
// 404 at the edge) — nothing is written, no phantom prazo/tasks/events.
func TestDeadline_Confirm_NoPrazo_NotFound(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)
	p := seedDeadlineParentsCommitted(ctx, t, pool)
	uc := newDeadlineUC(pool)

	cmd := deadline.ConfirmCommand{
		TenantID:     p.tenantID.String(),
		UserID:       uuid.NewString(),
		IntimationID: p.intimationID.String(), // seeded intimação, but NO prazo derived yet
		Kind:         deadline.KindGenerico,
		Days:         5,
		Counting:     deadline.CountingBusiness,
	}
	_, err := uc.Confirm(ctx, cmd)
	if err == nil {
		t.Fatal("Confirm() error = nil, want ErrDeadlineNotFound")
	}

	var deadlines, tasks int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM deadline WHERE notification_id = $1`, p.intimationID).Scan(&deadlines); err != nil {
		t.Fatalf("count deadline: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM task WHERE intimation_id = $1`, p.intimationID).Scan(&tasks); err != nil {
		t.Fatalf("count task: %v", err)
	}
	if deadlines != 0 || tasks != 0 {
		t.Errorf("deadline/task rows = %d/%d, want 0/0 (nothing to confirm)", deadlines, tasks)
	}
}

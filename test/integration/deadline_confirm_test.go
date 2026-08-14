//go:build integration

// Deadline confirm integration tests — prove the F2 "Aprovar tudo" write path (slice 5a,
// docs/erd-prazos.md §9) end to end against a REAL Postgres: seed a PENDING prazo via the
// creation path, then Confirm it and assert — in ONE tx — the prazo flips to OPEN with the
// RECOMPUTED end_date (through the real judicial calendar) + confirmed_by/at and a
// deadline.updated commits to the outbox. The confirm NEVER touches tasks: those are managed
// via POST/PATCH /v1/tasks (the "Análise" section), so a confirm must not create — nor delete —
// task rows. These drive the real use case (real repo + calendar + outbox + uow), the same
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
// (10 BUSINESS). The prazo must flip to OPEN with a RECOMPUTED end (2024-03-18, not the
// PENDING 2024-03-11), stamped confirmed_by/at, with a single deadline.updated committed — and
// NO tasks nor task.created, since the confirm no longer owns the task lifecycle.
func TestDeadline_Confirm_FlipsOpenRecomputes(t *testing.T) {
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

	// The human confirms: CONTESTACAO, 10 dias úteis. No tasks — those are managed via /v1/tasks.
	userID := uuid.NewString()
	cmd := deadline.ConfirmCommand{
		TenantID:     p.tenantID.String(),
		UserID:       userID,
		IntimationID: p.intimationID.String(),
		Kind:         deadline.KindContestacao,
		Days:         10,
		Counting:     deadline.CountingBusiness,
	}
	res, err := uc.Confirm(ctx, cmd)
	if err != nil {
		t.Fatalf("Confirm() error = %v", err)
	}
	if res.Deadline.ID != deadlineID {
		t.Errorf("result deadline = %q, want %q", res.Deadline.ID, deadlineID)
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

	// The confirm created NO tasks — the task lifecycle lives in /v1/tasks.
	var taskCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM task WHERE deadline_id = $1 AND tenant_id = $2`,
		deadlineID, p.tenantID).Scan(&taskCount); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if taskCount != 0 {
		t.Errorf("task rows = %d, want 0 (confirm never creates tasks)", taskCount)
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

	// The confirm emitted NO task.created — it does not create tasks.
	var taskCreatedCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM outbox
		WHERE type = $1 AND aggregate_type = 'task' AND payload->>'deadline_id' = $2`,
		deadline.TypeTaskCreated, deadlineID).Scan(&taskCreatedCount); err != nil {
		t.Fatalf("count task.created: %v", err)
	}
	if taskCreatedCount != 0 {
		t.Errorf("task.created rows = %d, want 0 (confirm never creates tasks)", taskCreatedCount)
	}
}

// DLC3 is the bug's end-to-end regression: tasks created via the Análise section (POST
// /v1/tasks) MUST survive a confirm — the confirm no longer deletes the prazo's tasks. Seed a
// PENDING prazo, create 2 tasks tied to it via CreateTask, then confirm the prazo TWICE and
// assert the 2 tasks still exist (never dropped to 0, the data-loss the fix prevents).
func TestDeadline_Confirm_DoesNotDeleteExistingTasks(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)
	p := seedDeadlineParentsCommitted(ctx, t, pool)
	uc := newDeadlineUC(pool)

	// Seed the PENDING prazo (the F2 suggestion) via the creation path.
	obs := observedFor(p, uuid.NewString(), "INTIMACAO", "TJSP", "SP", "2024-03-04")
	if err := uc.OnIntimationObserved(ctx, obs); err != nil {
		t.Fatalf("OnIntimationObserved() error = %v", err)
	}

	var deadlineID string
	if err := pool.QueryRow(ctx,
		`SELECT id FROM deadline WHERE notification_id = $1`, p.intimationID).Scan(&deadlineID); err != nil {
		t.Fatalf("read pending deadline: %v", err)
	}

	// The lawyer creates 2 tasks for the prazo via the Análise section (POST /v1/tasks).
	tenant := p.tenantID.String()
	userID := uuid.NewString()
	for _, title := range []string{"Protocolar contestação", "Dar ciência ao cliente"} {
		if _, err := uc.CreateTask(ctx, deadline.CreateTaskCommand{
			TenantID:      tenant,
			UserID:        userID,
			CourtRecordID: p.courtRecordID.String(),
			DeadlineID:    deadlineID,
			IntimationID:  p.intimationID.String(),
			Title:         title,
		}); err != nil {
			t.Fatalf("CreateTask(%q): %v", title, err)
		}
	}

	cmd := deadline.ConfirmCommand{
		TenantID:     tenant,
		UserID:       userID,
		IntimationID: p.intimationID.String(),
		Kind:         deadline.KindContestacao,
		Days:         10,
		Counting:     deadline.CountingBusiness,
	}

	// Confirm the same intimação TWICE — the tasks must survive both confirms untouched.
	for i := 0; i < 2; i++ {
		if _, err := uc.Confirm(ctx, cmd); err != nil {
			t.Fatalf("Confirm() #%d error = %v", i, err)
		}
		var taskCount int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM task WHERE deadline_id = $1 AND tenant_id = $2`,
			deadlineID, p.tenantID).Scan(&taskCount); err != nil {
			t.Fatalf("count tasks after confirm #%d: %v", i, err)
		}
		if taskCount != 2 {
			t.Errorf("task rows after confirm #%d = %d, want 2 (confirm must not delete tasks)", i, taskCount)
		}
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

//go:build integration

// Task read + write integration tests (slice 5b, docs/erd-prazos.md §9/§10) — prove the task
// surface of the deadline slice end to end against a REAL Postgres with every migration applied:
// tasks are SEEDED both through the F2 confirm (the N approved tasks) and the manual CREATE (POST
// /v1/tasks), then read back through the real read models (GET /v1/processos/:id/tasks, GET
// /v1/tasks with the assignee "meus" filter) and transitioned (done stamps completed_at,
// dismiss does not). These drive the real use case (real repo + calendar + outbox + uow) and the
// pool-backed read use case — the same composition cmd/api mounts. Each test uses a fresh tenant,
// so the tenant filter (barrier 1) isolates counts on the shared DB.
package integration_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jusassessoria/platform/internal/deadline"
)

// firstPageTasksByProcesso reads the process's Tasks tab from the min sentinel cursor.
func firstPageTasksByProcesso(ctx context.Context, t *testing.T, reader *deadline.ReadUseCase, tenant, courtRecordID string) deadline.TasksByProcessoResult {
	t.Helper()
	res, err := reader.TasksByProcesso(ctx, deadline.TasksByProcessoQuery{
		TenantID: tenant, CourtRecordID: courtRecordID,
		LastDue: "0001-01-01", LastID: "00000000-0000-0000-0000-000000000000", Limit: 20,
	})
	if err != nil {
		t.Fatalf("TasksByProcesso: %v", err)
	}
	return res
}

// TSK1: confirm a prazo, then create two tasks for it via CreateTask (one assigned to userA),
// then read them back — the process tab returns both, and the agenda filtered by assignee=userA
// ("meus prazos") returns only the assigned one. Proves the read models + the assignee filter
// over genuinely-written task rows.
func TestTasks_Read_TabAndAgendaAssigneeFilter(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)
	p := seedDeadlineParentsCommitted(ctx, t, pool)
	uc := newDeadlineUC(pool)
	reader := newDeadlineReader(pool)
	tenant := p.tenantID.String()

	// Seed the PENDING prazo and confirm it (no tasks — those come from POST /v1/tasks).
	obs := observedFor(p, uuid.NewString(), "INTIMACAO", "TJSP", "SP", "2024-03-04")
	if err := uc.OnIntimationObserved(ctx, obs); err != nil {
		t.Fatalf("OnIntimationObserved: %v", err)
	}
	var deadlineID string
	if err := pool.QueryRow(ctx,
		`SELECT id FROM deadline WHERE notification_id = $1`, p.intimationID).Scan(&deadlineID); err != nil {
		t.Fatalf("read pending deadline: %v", err)
	}
	if _, err := uc.Confirm(ctx, deadline.ConfirmCommand{
		TenantID:     tenant,
		UserID:       uuid.NewString(),
		IntimationID: p.intimationID.String(),
		Kind:         deadline.KindContestacao,
		Days:         10,
		Counting:     deadline.CountingBusiness,
	}); err != nil {
		t.Fatalf("Confirm: %v", err)
	}

	// Create two tasks for the prazo (the Análise section, POST /v1/tasks): one assigned to
	// userA + dated, one bare.
	userA := uuid.NewString()
	dueA := mustDate(t, "2024-03-10")
	if _, err := uc.CreateTask(ctx, deadline.CreateTaskCommand{
		TenantID: tenant, UserID: uuid.NewString(), CourtRecordID: p.courtRecordID.String(),
		DeadlineID: deadlineID, IntimationID: p.intimationID.String(),
		Title: "Protocolar contestação", Kind: "PECA", DueDate: &dueA, AssigneeUserID: userA,
	}); err != nil {
		t.Fatalf("CreateTask (assigned): %v", err)
	}
	if _, err := uc.CreateTask(ctx, deadline.CreateTaskCommand{
		TenantID: tenant, UserID: uuid.NewString(), CourtRecordID: p.courtRecordID.String(),
		DeadlineID: deadlineID, IntimationID: p.intimationID.String(),
		Title: "Dar ciência ao cliente",
	}); err != nil {
		t.Fatalf("CreateTask (bare): %v", err)
	}

	// The process tab returns both tasks (all statuses, no filter).
	tab := firstPageTasksByProcesso(ctx, t, reader, tenant, p.courtRecordID.String())
	if len(tab.Items) != 2 || tab.Total != 2 {
		t.Fatalf("tab items/total = %d/%d, want 2/2", len(tab.Items), tab.Total)
	}
	// Soonest-due first: the dated task leads, the undated one trails (NULLS LAST).
	if tab.Items[0].Title != "Protocolar contestação" || tab.Items[0].DueDate == nil {
		t.Errorf("first tab task = %q (due %v), want the dated 'Protocolar contestação'", tab.Items[0].Title, tab.Items[0].DueDate)
	}
	if tab.Items[1].DueDate != nil {
		t.Errorf("second tab task due = %v, want nil (undated sorts last)", tab.Items[1].DueDate)
	}

	// The agenda filtered by assignee=userA ("meus") returns only the assigned task.
	mine, err := reader.Tasks(ctx, deadline.TasksQuery{
		TenantID: tenant, Assignee: userA,
		LastDue: "0001-01-01", LastID: "00000000-0000-0000-0000-000000000000", Limit: 20,
	})
	if err != nil {
		t.Fatalf("Tasks (assignee filter): %v", err)
	}
	if len(mine.Items) != 1 || mine.Items[0].AssigneeUserID != userA {
		t.Fatalf("assignee-filtered agenda = %d items (first assignee %q), want 1 for %q", len(mine.Items), ids0(mine.Items), userA)
	}
	// "X de Y": X (filtered) is 1, Y (tenant-wide) is 2.
	if mine.TotalCount != 1 || mine.Total != 2 {
		t.Errorf("agenda totals = %d/%d, want 1/2 (X filtered de Y tenant)", mine.TotalCount, mine.Total)
	}
}

// TSK2: a manual CREATE (POST /v1/tasks) writes an OPEN/MANUAL task, done flips it to DONE
// stamping completed_at + emits task.completed, and dismiss on a second task flips it to
// DISMISSED WITHOUT completed_at + emits task.dismissed. Proves the write path + the guards end
// to end.
func TestTasks_CreateThenDoneAndDismiss(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)
	p := seedDeadlineParentsCommitted(ctx, t, pool)
	uc := newDeadlineUC(pool)
	tenant := p.tenantID.String()
	userID := uuid.NewString()

	// Create two manual tasks tied to the process (court_record from the seeded parents).
	mk := func(title string) string {
		task, err := uc.CreateTask(ctx, deadline.CreateTaskCommand{
			TenantID: tenant, UserID: userID, CourtRecordID: p.courtRecordID.String(), Title: title,
		})
		if err != nil {
			t.Fatalf("CreateTask(%q): %v", title, err)
		}
		return task.ID
	}
	doneID := mk("Concluir esta")
	dismissID := mk("Dispensar esta")

	// Both land OPEN/MANUAL with created_by set.
	var openCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM task WHERE tenant_id = $1 AND status = 'OPEN' AND source = 'MANUAL' AND created_by = $2`,
		p.tenantID, userID).Scan(&openCount); err != nil {
		t.Fatalf("count created tasks: %v", err)
	}
	if openCount != 2 {
		t.Fatalf("created OPEN/MANUAL tasks = %d, want 2", openCount)
	}

	// done: OPEN→DONE, completed_at stamped, task.completed emitted.
	res, err := uc.MarkTaskDone(ctx, tenant, userID, doneID)
	if err != nil {
		t.Fatalf("MarkTaskDone: %v", err)
	}
	if res.Status != deadline.TaskStatusDone {
		t.Errorf("done status = %q, want DONE", res.Status)
	}
	var doneStatus string
	var doneCompleted *string
	if err := pool.QueryRow(ctx,
		`SELECT status, completed_at::text FROM task WHERE id = $1`, doneID).Scan(&doneStatus, &doneCompleted); err != nil {
		t.Fatalf("read done task: %v", err)
	}
	if doneStatus != "DONE" || doneCompleted == nil {
		t.Errorf("done task status/completed_at = %q/%v, want DONE/<a timestamp>", doneStatus, doneCompleted)
	}
	assertTaskEvent(ctx, t, pool, deadline.TypeTaskCompleted, doneID)

	// dismiss: OPEN→DISMISSED, completed_at left NULL, task.dismissed emitted.
	if _, err := uc.DismissTask(ctx, tenant, userID, dismissID); err != nil {
		t.Fatalf("DismissTask: %v", err)
	}
	var dismissStatus string
	var dismissCompleted *string
	if err := pool.QueryRow(ctx,
		`SELECT status, completed_at::text FROM task WHERE id = $1`, dismissID).Scan(&dismissStatus, &dismissCompleted); err != nil {
		t.Fatalf("read dismissed task: %v", err)
	}
	if dismissStatus != "DISMISSED" || dismissCompleted != nil {
		t.Errorf("dismissed task status/completed_at = %q/%v, want DISMISSED/NULL", dismissStatus, dismissCompleted)
	}
	assertTaskEvent(ctx, t, pool, deadline.TypeTaskDismissed, dismissID)

	// Re-doing a terminal task is refused (ErrTaskNotOpen) — the guard holds end to end.
	if _, err := uc.MarkTaskDone(ctx, tenant, userID, doneID); err == nil {
		t.Error("MarkTaskDone on a DONE task = nil error, want ErrTaskNotOpen")
	}
}

// TSK3: PATCH edits a task's fields in place and emits task.updated; the edit never changes the
// status, and a foreign tenant never sees the task (typed not-found).
func TestTasks_Update(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)
	p := seedDeadlineParentsCommitted(ctx, t, pool)
	uc := newDeadlineUC(pool)
	tenant := p.tenantID.String()

	task, err := uc.CreateTask(ctx, deadline.CreateTaskCommand{
		TenantID: tenant, UserID: uuid.NewString(), Title: "antigo",
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	newTitle := "novo título"
	assignee := uuid.NewString()
	saved, err := uc.UpdateTask(ctx, deadline.UpdateTaskCommand{
		TenantID: tenant, UserID: uuid.NewString(), TaskID: task.ID, Title: &newTitle, AssigneeUserID: &assignee,
	})
	if err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	if saved.Title != newTitle || saved.AssigneeUserID != assignee || saved.Status != deadline.TaskStatusOpen {
		t.Errorf("saved = %+v, want title/assignee patched + status OPEN (unchanged)", saved)
	}
	var title, assigneeCol string
	if err := pool.QueryRow(ctx,
		`SELECT title, assignee_user_id::text FROM task WHERE id = $1`, task.ID).Scan(&title, &assigneeCol); err != nil {
		t.Fatalf("read updated task: %v", err)
	}
	if title != newTitle || assigneeCol != assignee {
		t.Errorf("persisted title/assignee = %q/%q, want %q/%q", title, assigneeCol, newTitle, assignee)
	}
	assertTaskEvent(ctx, t, pool, deadline.TypeTaskUpdated, task.ID)

	// A foreign tenant patching the task is a typed not-found (RLS + barrier 1).
	if _, err := uc.UpdateTask(ctx, deadline.UpdateTaskCommand{
		TenantID: uuid.NewString(), TaskID: task.ID, Title: &newTitle,
	}); err == nil {
		t.Error("cross-tenant UpdateTask = nil error, want ErrTaskNotFound")
	}
}

// assertTaskEvent asserts exactly one outbox row of the given task event type exists on the task
// aggregate — the transactional-outbox commit landed with the write.
func assertTaskEvent(ctx context.Context, t *testing.T, pool *pgxpool.Pool, eventType, taskID string) {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM outbox WHERE type = $1 AND aggregate_type = 'task' AND aggregate_id = $2`,
		eventType, taskID).Scan(&count); err != nil {
		t.Fatalf("count %s: %v", eventType, err)
	}
	if count != 1 {
		t.Errorf("%s rows for task %s = %d, want 1", eventType, taskID, count)
	}
}

// ids0 is a tiny debug helper: the assignee of the first agenda task (for failure messages).
func ids0(items []deadline.TaskView) string {
	if len(items) == 0 {
		return "<none>"
	}
	return items[0].AssigneeUserID
}

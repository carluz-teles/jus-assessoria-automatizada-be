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

// TSK4: a DISMISSED task must never inflate the agenda's "X de Y" counter beyond what
// GET /v1/tasks actually returns — regression for the bug where CountTasks/CountTasksByTenant
// counted DISMISSED rows while ListTasks always excludes them (t.status <> 'DISMISSED'
// unconditionally), so `total_count > 0` came back against an empty `data` page whenever
// ?status=DISMISSED was requested (and, more subtly, total_count/total ran too high even with
// no filter, since a dismissed task was always absent from Items but always present in the
// count). Seeds one OPEN + one DISMISSED task on the same tenant and asserts len(Items) ==
// TotalCount == Total in two scenarios: no status filter, and an explicit ?status=DISMISSED.
func TestTasks_DismissedNeverInflatesTotalCount(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)
	p := seedDeadlineParentsCommitted(ctx, t, pool)
	uc := newDeadlineUC(pool)
	reader := newDeadlineReader(pool)
	tenant := p.tenantID.String()
	userID := uuid.NewString()

	openTask, err := uc.CreateTask(ctx, deadline.CreateTaskCommand{
		TenantID: tenant, UserID: userID, Title: "fica aberta",
	})
	if err != nil {
		t.Fatalf("CreateTask (open): %v", err)
	}
	dismissedTask, err := uc.CreateTask(ctx, deadline.CreateTaskCommand{
		TenantID: tenant, UserID: userID, Title: "vai ser dispensada",
	})
	if err != nil {
		t.Fatalf("CreateTask (to dismiss): %v", err)
	}
	if _, err := uc.DismissTask(ctx, tenant, userID, dismissedTask.ID); err != nil {
		t.Fatalf("DismissTask: %v", err)
	}

	// Scenario 1: no status filter — the DISMISSED task is excluded from BOTH the page and
	// the totals, so only the OPEN task counts.
	all, err := reader.Tasks(ctx, deadline.TasksQuery{
		TenantID: tenant, LastDue: "0001-01-01", LastID: "00000000-0000-0000-0000-000000000000", Limit: 20,
	})
	if err != nil {
		t.Fatalf("Tasks (no filter): %v", err)
	}
	if len(all.Items) != 1 || all.Items[0].ID != openTask.ID {
		t.Fatalf("unfiltered items = %d (first id %q), want 1 item = the OPEN task %q", len(all.Items), ids0(all.Items), openTask.ID)
	}
	if all.TotalCount != int64(len(all.Items)) || all.Total != int64(len(all.Items)) {
		t.Errorf("unfiltered totals = %d/%d, want both == len(data) = %d", all.TotalCount, all.Total, len(all.Items))
	}

	// Scenario 2: an explicit ?status=DISMISSED filter — ListTasks' unconditional exclusion
	// makes this contradictory (status <> 'DISMISSED' AND status = 'DISMISSED'), so both the
	// page and the count resolve to zero, consistently.
	dismissedOnly, err := reader.Tasks(ctx, deadline.TasksQuery{
		TenantID: tenant, Status: string(deadline.TaskStatusDismissed),
		LastDue: "0001-01-01", LastID: "00000000-0000-0000-0000-000000000000", Limit: 20,
	})
	if err != nil {
		t.Fatalf("Tasks (status=DISMISSED): %v", err)
	}
	if len(dismissedOnly.Items) != 0 {
		t.Fatalf("status=DISMISSED items = %d, want 0", len(dismissedOnly.Items))
	}
	if dismissedOnly.TotalCount != 0 {
		t.Errorf("status=DISMISSED total_count = %d, want 0 (must match the empty data page)", dismissedOnly.TotalCount)
	}
}

// ids0 is a tiny debug helper: the assignee of the first agenda task (for failure messages).
func ids0(items []deadline.TaskView) string {
	if len(items) == 0 {
		return "<none>"
	}
	return items[0].AssigneeUserID
}

// TestTasks_ExcludesOpenTaskOnArchivedProcess (Achado 2, fatia 2c — "Meus Prazos"/"Pipeline"
// coverage): an OPEN task whose court_record is already ARCHIVED leaves the fila ativa (the
// agenda list, its "X de Y" counters, and the KPI summary), the SAME exclusion
// internal/acquisition already applies to a PENDING intimação. A DONE task on the SAME
// archived process stays visible — the exclusion targets active/OPEN work, not the whole
// archived process (mirrors how a RESOLVED intimação stays visible). An avulsa task (no
// court_record_id) is never affected by the null-safe predicate. The process's own Tasks tab
// (TasksByProcesso, the detail/history view) is NOT filtered — it keeps showing everything,
// exactly like GetIntimacao/ListIntimacoesByProcesso for intimações.
func TestTasks_ExcludesOpenTaskOnArchivedProcess(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)
	uc := newDeadlineUC(pool)
	reader := newDeadlineReader(pool)

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-tasks-fila-ativa", 0)

	activeRecord, _ := seedCourtRecordCNJ(t, pool, tenantID, "0000015-00.2024.8.26.0015")
	archivedRecord, _ := seedCourtRecordCNJWithLifecycle(t, pool, tenantID, "0000016-00.2024.8.26.0016", "ARCHIVED")

	activeOpen, err := uc.CreateTask(ctx, deadline.CreateTaskCommand{
		TenantID: tenantID, UserID: uuid.NewString(), CourtRecordID: activeRecord, Title: "Ativo aberta",
	})
	if err != nil {
		t.Fatalf("CreateTask (active, open): %v", err)
	}
	archivedOpen, err := uc.CreateTask(ctx, deadline.CreateTaskCommand{
		TenantID: tenantID, UserID: uuid.NewString(), CourtRecordID: archivedRecord, Title: "Arquivada aberta",
	})
	if err != nil {
		t.Fatalf("CreateTask (archived, open): %v", err)
	}
	archivedDone, err := uc.CreateTask(ctx, deadline.CreateTaskCommand{
		TenantID: tenantID, UserID: uuid.NewString(), CourtRecordID: archivedRecord, Title: "Arquivada concluída",
	})
	if err != nil {
		t.Fatalf("CreateTask (archived, to-be-done): %v", err)
	}
	if _, err := uc.MarkTaskDone(ctx, tenantID, uuid.NewString(), archivedDone.ID); err != nil {
		t.Fatalf("MarkTaskDone: %v", err)
	}
	avulsa, err := uc.CreateTask(ctx, deadline.CreateTaskCommand{
		TenantID: tenantID, UserID: uuid.NewString(), Title: "Avulsa sem processo",
	})
	if err != nil {
		t.Fatalf("CreateTask (avulsa): %v", err)
	}

	// The agenda (GET /v1/tasks, no filter): active-open + archived-done + avulsa are visible;
	// archived-open is excluded.
	res, err := reader.Tasks(ctx, deadline.TasksQuery{
		TenantID: tenantID, LastDue: "0001-01-01", LastID: "00000000-0000-0000-0000-000000000000", Limit: 50,
	})
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	seen := map[string]bool{}
	for _, item := range res.Items {
		seen[item.ID] = true
	}
	if !seen[activeOpen.ID] {
		t.Error("active-process OPEN task missing from the agenda")
	}
	if seen[archivedOpen.ID] {
		t.Error("archived-process OPEN task still appears in the agenda, want excluded")
	}
	if !seen[archivedDone.ID] {
		t.Error("archived-process DONE task missing — the exclusion must target OPEN only")
	}
	if !seen[avulsa.ID] {
		t.Error("avulsa task (no court_record) missing — the null-safe predicate must never exclude it")
	}

	// "X de Y": both must agree with the visible set (3 tasks: active-open, archived-done,
	// avulsa), not 4 (the excluded archived-open must not inflate either counter).
	if res.TotalCount != 3 || res.Total != 3 {
		t.Errorf("agenda totals = %d/%d, want 3/3 (archived-process OPEN task excluded from both)", res.TotalCount, res.Total)
	}

	// The KPI summary (GET /v1/tasks/summary): abertas counts the active-process OPEN task
	// AND the avulsa one (no court_record to exclude on) but NOT the archived-process OPEN
	// task; concluidas counts the archived-process DONE task (untouched).
	summary, err := reader.TasksSummary(ctx, tenantID)
	if err != nil {
		t.Fatalf("TasksSummary: %v", err)
	}
	if summary.Abertas != 2 {
		t.Errorf("summary.abertas = %d, want 2 (active-process OPEN + avulsa, archived-process OPEN excluded)", summary.Abertas)
	}
	if summary.Concluidas != 1 {
		t.Errorf("summary.concluidas = %d, want 1 (the archived-process DONE task, unaffected)", summary.Concluidas)
	}

	// The process's own Tasks tab (detail/history) is NOT filtered — the archived-open task
	// stays reachable there, exactly like GetIntimacao/ListIntimacoesByProcesso for intimações.
	tab := firstPageTasksByProcesso(ctx, t, reader, tenantID, archivedRecord)
	var tabHasOpen bool
	for _, item := range tab.Items {
		if item.ID == archivedOpen.ID {
			tabHasOpen = true
		}
	}
	if !tabHasOpen {
		t.Error("archived-process OPEN task missing from its own process Tasks tab — detail/history must stay unfiltered")
	}
}

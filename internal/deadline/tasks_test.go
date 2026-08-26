package deadline

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jusassessoria/platform/lib/apperr"
)

// --- CreateTask -------------------------------------------------------------

// TestCreateTask_InsertsManualOpenAndEmits is the happy path for POST /v1/tasks: a manual task is
// persisted OPEN/MANUAL/created_by=principal carrying the (optional) context ids, in a
// tenant-scoped tx, and exactly one task.created is emitted with a parseable uuid aggregate.
func TestCreateTask_InsertsManualOpenAndEmits(t *testing.T) {
	tenantID := uuid.NewString()
	userID := uuid.NewString()
	deadlineID := uuid.NewString()
	courtRecordID := uuid.NewString()
	assignee := uuid.NewString()
	due := time.Date(2024, 3, 15, 0, 0, 0, 0, time.UTC)

	repo := &mockRepo{deadlineEndDate: time.Date(2024, 3, 31, 0, 0, 0, 0, time.UTC)}
	outbox := &fakeOutbox{}
	uow := &fakeUOW{}
	uc := NewUseCase(repo, &fakeCalendar{}, outbox, &fakeDedup{}, uow)

	task, err := uc.CreateTask(context.Background(), CreateTaskCommand{
		TenantID: tenantID, UserID: userID,
		CourtRecordID: courtRecordID, DeadlineID: deadlineID,
		Title: "Peça", Kind: "PECA", Description: "minutar", DueDate: &due, AssigneeUserID: assignee,
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}

	if len(uow.scopes) != 1 || uow.scopes[0] != tenantID {
		t.Errorf("uow scopes = %v, want [%q]", uow.scopes, tenantID)
	}
	if repo.insertTaskCalls != 1 || len(repo.insertedTasks) != 1 {
		t.Fatalf("InsertTask calls = %d, want 1", repo.insertTaskCalls)
	}
	saved := repo.insertedTasks[0]
	if saved.Status != TaskStatusOpen || saved.Source != SourceManual || saved.CreatedBy != userID {
		t.Errorf("task status/source/created_by = %q/%q/%q, want OPEN/MANUAL/%q", saved.Status, saved.Source, saved.CreatedBy, userID)
	}
	if saved.TenantID != tenantID || saved.DeadlineID != deadlineID || saved.CourtRecordID != courtRecordID {
		t.Error("task context ids (tenant/deadline/court_record) not carried")
	}
	if saved.AssigneeUserID != assignee || saved.DueDate == nil || !saved.DueDate.Equal(due) {
		t.Errorf("task assignee/due = %q/%v, want %q/%v", saved.AssigneeUserID, saved.DueDate, assignee, due)
	}
	if task.ID == "" {
		t.Error("returned task has no id")
	}

	created := publishedOfType[TaskCreated](outbox)
	if len(created) != 1 {
		t.Fatalf("task.created events = %d, want 1", len(created))
	}
	tc := created[0]
	if tc.Type() != TypeTaskCreated || tc.AggregateType() != aggregateTypeTask || tc.AggregateID() != task.ID {
		t.Errorf("task.created type/aggregate = %q/%q/%q", tc.Type(), tc.AggregateType(), tc.AggregateID())
	}
	if _, err := uuid.Parse(tc.AggregateID()); err != nil {
		t.Errorf("task.created aggregate is not a uuid: %v", err)
	}
	if tc.DeadlineID != deadlineID || tc.CourtRecordID != courtRecordID || tc.AssigneeUserID != assignee {
		t.Errorf("task.created payload = %+v", tc)
	}
}

// TestCreateTask_AvulsaTaskHasNoContext proves a task with no deadline/process (avulsa) inserts
// with empty context ids and still emits task.created — the manual backlog case.
func TestCreateTask_AvulsaTaskHasNoContext(t *testing.T) {
	repo := &mockRepo{}
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, &fakeCalendar{}, outbox, &fakeDedup{}, &fakeUOW{})

	if _, err := uc.CreateTask(context.Background(), CreateTaskCommand{
		TenantID: uuid.NewString(), UserID: uuid.NewString(), Title: "Ligar para o cliente",
	}); err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	saved := repo.insertedTasks[0]
	if saved.DeadlineID != "" || saved.CourtRecordID != "" || saved.IntimationID != "" || saved.DueDate != nil {
		t.Errorf("avulsa task carried context = deadline %q / cr %q / intim %q / due %v, want all empty",
			saved.DeadlineID, saved.CourtRecordID, saved.IntimationID, saved.DueDate)
	}
	if len(publishedOfType[TaskCreated](outbox)) != 1 {
		t.Error("task.created not emitted for an avulsa task")
	}
	if repo.intimationAssignCalls != 0 {
		t.Errorf("GetIntimationAssignee calls = %d, want 0 (no intimation_id, regression guard)", repo.intimationAssignCalls)
	}
}

// TestCreateTask_InheritsAssigneeFromIntimation is the happy path of herança intimação →
// tarefa: intimation_id is set and assignee_user_id is left empty, so CreateTask snapshots
// the intimação's vigente assignee onto the new task at CREATE time.
func TestCreateTask_InheritsAssigneeFromIntimation(t *testing.T) {
	tenantID := uuid.NewString()
	intimationID := uuid.NewString()
	inheritedUser := uuid.NewString()

	repo := &mockRepo{intimationAssignee: &inheritedUser}
	uc := NewUseCase(repo, &fakeCalendar{}, &fakeOutbox{}, &fakeDedup{}, &fakeUOW{})

	task, err := uc.CreateTask(context.Background(), CreateTaskCommand{
		TenantID: tenantID, UserID: uuid.NewString(), IntimationID: intimationID,
		Title: "Peça",
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	if repo.intimationAssignCalls != 1 || repo.gotIntimationAssignID != intimationID {
		t.Fatalf("GetIntimationAssignee calls/id = %d/%q, want 1/%q", repo.intimationAssignCalls, repo.gotIntimationAssignID, intimationID)
	}
	if task.AssigneeUserID != inheritedUser {
		t.Errorf("task.AssigneeUserID = %q, want inherited %q", task.AssigneeUserID, inheritedUser)
	}
}

// TestCreateTask_ExplicitAssigneeWinsOverIntimation proves an explicit assignee_user_id in
// the command always wins over the intimação's — the herança is a fallback for an empty
// field, never an override of an explicit choice.
func TestCreateTask_ExplicitAssigneeWinsOverIntimation(t *testing.T) {
	intimationID := uuid.NewString()
	explicitUser := uuid.NewString()
	inheritedUser := uuid.NewString()

	repo := &mockRepo{intimationAssignee: &inheritedUser}
	uc := NewUseCase(repo, &fakeCalendar{}, &fakeOutbox{}, &fakeDedup{}, &fakeUOW{})

	task, err := uc.CreateTask(context.Background(), CreateTaskCommand{
		TenantID: uuid.NewString(), UserID: uuid.NewString(), IntimationID: intimationID,
		Title: "Peça", AssigneeUserID: explicitUser,
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	if repo.intimationAssignCalls != 0 {
		t.Errorf("GetIntimationAssignee calls = %d, want 0 (explicit assignee skips the lookup)", repo.intimationAssignCalls)
	}
	if task.AssigneeUserID != explicitUser {
		t.Errorf("task.AssigneeUserID = %q, want the explicit %q", task.AssigneeUserID, explicitUser)
	}
}

// TestCreateTask_IntimationWithoutAssignee_TaskStaysUnassigned proves a nil
// GetIntimationAssignee answer (the intimação has no responsável) is not an error — the
// task is created OPEN with an empty AssigneeUserID.
func TestCreateTask_IntimationWithoutAssignee_TaskStaysUnassigned(t *testing.T) {
	intimationID := uuid.NewString()

	repo := &mockRepo{intimationAssignee: nil}
	uc := NewUseCase(repo, &fakeCalendar{}, &fakeOutbox{}, &fakeDedup{}, &fakeUOW{})

	task, err := uc.CreateTask(context.Background(), CreateTaskCommand{
		TenantID: uuid.NewString(), UserID: uuid.NewString(), IntimationID: intimationID,
		Title: "Peça",
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	if task.AssigneeUserID != "" {
		t.Errorf("task.AssigneeUserID = %q, want empty (intimação has no responsável)", task.AssigneeUserID)
	}
}

// TestCreateTask_DueDateWithinDeadline is the happy path of the task invariant (ERD §4): a task
// linked to a prazo whose due_date is on/before the prazo's end_date inserts normally and emits
// task.created. The narrow GetDeadlineEndDate read happens inside the tx, tenant-scoped.
func TestCreateTask_DueDateWithinDeadline(t *testing.T) {
	tenantID := uuid.NewString()
	deadlineID := uuid.NewString()
	end := time.Date(2024, 3, 20, 0, 0, 0, 0, time.UTC)
	due := time.Date(2024, 3, 15, 0, 0, 0, 0, time.UTC)

	repo := &mockRepo{deadlineEndDate: end}
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, &fakeCalendar{}, outbox, &fakeDedup{}, &fakeUOW{})

	if _, err := uc.CreateTask(context.Background(), CreateTaskCommand{
		TenantID: tenantID, UserID: uuid.NewString(), DeadlineID: deadlineID,
		Title: "Peça", DueDate: &due,
	}); err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}
	if repo.deadlineEndDateCalls != 1 || repo.gotDeadlineEndDateID != deadlineID || repo.gotDeadlineEndDateTenant != tenantID {
		t.Errorf("GetDeadlineEndDate calls/id/tenant = %d/%q/%q, want 1/%q/%q",
			repo.deadlineEndDateCalls, repo.gotDeadlineEndDateID, repo.gotDeadlineEndDateTenant, deadlineID, tenantID)
	}
	if repo.insertTaskCalls != 1 || len(publishedOfType[TaskCreated](outbox)) != 1 {
		t.Errorf("InsertTask/published = %d/%d, want 1/1", repo.insertTaskCalls, len(publishedOfType[TaskCreated](outbox)))
	}
}

// TestCreateTask_DueDateAfterDeadlineEnd rejects a task whose due_date falls AFTER the prazo's
// end_date (ERD §4): the write is refused BEFORE persisting — nothing inserts, nothing emits —
// with a typed KindInvalid (→ 400) reporting the offending dates.
func TestCreateTask_DueDateAfterDeadlineEnd(t *testing.T) {
	end := time.Date(2024, 3, 20, 0, 0, 0, 0, time.UTC)
	due := time.Date(2024, 3, 25, 0, 0, 0, 0, time.UTC)

	repo := &mockRepo{deadlineEndDate: end}
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, &fakeCalendar{}, outbox, &fakeDedup{}, &fakeUOW{})

	_, err := uc.CreateTask(context.Background(), CreateTaskCommand{
		TenantID: uuid.NewString(), UserID: uuid.NewString(), DeadlineID: uuid.NewString(),
		Title: "Peça", DueDate: &due,
	})
	ae, ok := apperr.From(err)
	if !ok || ae.Kind != apperr.KindInvalid {
		t.Errorf("error = %v, want KindInvalid", err)
	}
	if repo.insertTaskCalls != 0 || len(outbox.published) != 0 {
		t.Errorf("insert/published = %d/%d, want 0/0 on a refused write", repo.insertTaskCalls, len(outbox.published))
	}
}

// TestCreateTask_DanglingDeadlineID fails the write: a task pointing at a deadline that is
// missing/foreign surfaces the repo's typed ErrDeadlineNotFound — a dangling deadline_id is a
// hard fault, never a silent acceptance.
func TestCreateTask_DanglingDeadlineID(t *testing.T) {
	due := time.Date(2024, 3, 15, 0, 0, 0, 0, time.UTC)
	repo := &mockRepo{deadlineEndDateErr: ErrDeadlineNotFound}
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, &fakeCalendar{}, outbox, &fakeDedup{}, &fakeUOW{})

	_, err := uc.CreateTask(context.Background(), CreateTaskCommand{
		TenantID: uuid.NewString(), UserID: uuid.NewString(), DeadlineID: uuid.NewString(),
		Title: "Peça", DueDate: &due,
	})
	ae, ok := apperr.From(err)
	if !ok || ae.Kind != apperr.KindNotFound {
		t.Errorf("error = %v, want KindNotFound", err)
	}
	if repo.insertTaskCalls != 0 || len(outbox.published) != 0 {
		t.Errorf("insert/published = %d/%d, want 0/0", repo.insertTaskCalls, len(outbox.published))
	}
}

// --- UpdateTask -------------------------------------------------------------

// TestUpdateTask_AppliesOnlyPresentFields is the core of the partial patch: a body with ONLY the
// title leaves description/kind/due_date/assignee at their stored values, in a tenant-scoped tx,
// and emits task.updated with a parseable uuid aggregate.
func TestUpdateTask_AppliesOnlyPresentFields(t *testing.T) {
	tenantID := uuid.NewString()
	taskID := uuid.NewString()
	due := time.Date(2024, 3, 15, 0, 0, 0, 0, time.UTC)

	repo := &mockRepo{
		taskForUpdate: &TaskForUpdate{
			ID: taskID, Status: TaskStatusOpen, Title: "antigo", Description: "desc", Kind: "PECA",
			DueDate: &due, AssigneeUserID: "u-old",
		},
		updatedTask: &Task{ID: taskID, Title: "novo", Status: TaskStatusOpen, Source: SourceManual},
	}
	outbox := &fakeOutbox{}
	uow := &fakeUOW{}
	uc := NewUseCase(repo, &fakeCalendar{}, outbox, &fakeDedup{}, uow)

	newTitle := "novo"
	saved, err := uc.UpdateTask(context.Background(), UpdateTaskCommand{
		TenantID: tenantID, TaskID: taskID, Title: &newTitle, // ONLY title present
	})
	if err != nil {
		t.Fatalf("UpdateTask() error = %v", err)
	}
	if saved.ID != taskID {
		t.Errorf("saved.ID = %q, want %q", saved.ID, taskID)
	}
	if len(uow.scopes) != 1 || uow.scopes[0] != tenantID {
		t.Errorf("uow scopes = %v, want [%q]", uow.scopes, tenantID)
	}
	if repo.gotTaskUpdateID != taskID || repo.gotTaskUpdateTenantID != tenantID {
		t.Errorf("GetTaskForUpdate id/tenant = %q/%q, want %q/%q", repo.gotTaskUpdateID, repo.gotTaskUpdateTenantID, taskID, tenantID)
	}

	up := repo.gotUpdateTaskParams
	if repo.updateTaskCalls != 1 {
		t.Fatalf("UpdateTask calls = %d, want 1", repo.updateTaskCalls)
	}
	if up.TaskID != taskID || up.TenantID != tenantID {
		t.Errorf("update keyed by task/tenant = %q/%q, want %q/%q", up.TaskID, up.TenantID, taskID, tenantID)
	}
	if up.Title != "novo" || up.Description != "desc" || up.Kind != "PECA" || up.AssigneeUserID != "u-old" {
		t.Errorf("merged fields = %+v, want title novo + the rest kept from storage", up)
	}
	if up.DueDate == nil || !up.DueDate.Equal(due) {
		t.Errorf("merged due_date = %v, want %v (kept)", up.DueDate, due)
	}

	updated := publishedOfType[TaskUpdated](outbox)
	if len(updated) != 1 {
		t.Fatalf("task.updated events = %d, want 1", len(updated))
	}
	u := updated[0]
	if u.Type() != TypeTaskUpdated || u.AggregateType() != aggregateTypeTask || u.AggregateID() != taskID || u.TaskID != taskID {
		t.Errorf("task.updated type/aggregate/task = %q/%q/%q/%q", u.Type(), u.AggregateType(), u.AggregateID(), u.TaskID)
	}
	if _, err := uuid.Parse(u.AggregateID()); err != nil {
		t.Errorf("task.updated aggregate is not a uuid: %v", err)
	}
}

// TestUpdateTask_ClearsDueDateAndAssignee proves a present "" clears the optional date/assignee
// (distinguished from an absent field, which keeps the stored value).
func TestUpdateTask_ClearsDueDateAndAssignee(t *testing.T) {
	taskID := uuid.NewString()
	due := time.Date(2024, 3, 15, 0, 0, 0, 0, time.UTC)
	repo := &mockRepo{
		taskForUpdate: &TaskForUpdate{ID: taskID, Status: TaskStatusOpen, Title: "t", DueDate: &due, AssigneeUserID: "u-old"},
		updatedTask:   &Task{ID: taskID, Title: "t", Status: TaskStatusOpen},
	}
	uc := NewUseCase(repo, &fakeCalendar{}, &fakeOutbox{}, &fakeDedup{}, &fakeUOW{})

	empty := ""
	if _, err := uc.UpdateTask(context.Background(), UpdateTaskCommand{
		TenantID: uuid.NewString(), TaskID: taskID, DueDate: &empty, AssigneeUserID: &empty,
	}); err != nil {
		t.Fatalf("UpdateTask() error = %v", err)
	}
	up := repo.gotUpdateTaskParams
	if up.DueDate != nil {
		t.Errorf("due_date = %v, want nil (cleared by present \"\")", up.DueDate)
	}
	if up.AssigneeUserID != "" {
		t.Errorf("assignee = %q, want \"\" (unassigned by present \"\")", up.AssigneeUserID)
	}
}

// TestUpdateTask_NotFound proves patching an unknown/foreign task is the repo's typed
// ErrTaskNotFound (→ 404): nothing is updated or emitted.
func TestUpdateTask_NotFound(t *testing.T) {
	repo := &mockRepo{taskForUpdateErr: ErrTaskNotFound}
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, &fakeCalendar{}, outbox, &fakeDedup{}, &fakeUOW{})

	title := "x"
	_, err := uc.UpdateTask(context.Background(), UpdateTaskCommand{
		TenantID: uuid.NewString(), TaskID: uuid.NewString(), Title: &title,
	})
	ae, ok := apperr.From(err)
	if !ok || ae.Kind != apperr.KindNotFound {
		t.Errorf("error = %v, want KindNotFound", err)
	}
	if repo.updateTaskCalls != 0 || len(outbox.published) != 0 {
		t.Errorf("update/published = %d/%d, want 0/0 on not-found", repo.updateTaskCalls, len(outbox.published))
	}
}

// TestUpdateTask_DueDateWithinDeadline proves PATCH can move a task's due_date anywhere on/before
// its prazo's end_date: the merged date is validated against the prazo's end_date (ERD §4) and the
// update proceeds, emitting task.updated. The deadline read is keyed by the task's OWN stored
// deadline_id, not the body.
func TestUpdateTask_DueDateWithinDeadline(t *testing.T) {
	tenantID := uuid.NewString()
	taskID := uuid.NewString()
	deadlineID := uuid.NewString()
	end := time.Date(2024, 3, 20, 0, 0, 0, 0, time.UTC)
	due := time.Date(2024, 3, 15, 0, 0, 0, 0, time.UTC)

	repo := &mockRepo{
		taskForUpdate: &TaskForUpdate{
			ID: taskID, Status: TaskStatusOpen, Title: "antigo", Kind: "PECA", DeadlineID: deadlineID,
		},
		deadlineEndDate: end,
		updatedTask:     &Task{ID: taskID, Title: "antigo", Status: TaskStatusOpen, Source: SourceManual},
	}
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, &fakeCalendar{}, outbox, &fakeDedup{}, &fakeUOW{})

	dueWire := due.Format("2006-01-02")
	if _, err := uc.UpdateTask(context.Background(), UpdateTaskCommand{
		TenantID: tenantID, TaskID: taskID, DueDate: &dueWire,
	}); err != nil {
		t.Fatalf("UpdateTask() error = %v", err)
	}
	if repo.deadlineEndDateCalls != 1 || repo.gotDeadlineEndDateID != deadlineID || repo.gotDeadlineEndDateTenant != tenantID {
		t.Errorf("GetDeadlineEndDate calls/id/tenant = %d/%q/%q, want 1/%q/%q",
			repo.deadlineEndDateCalls, repo.gotDeadlineEndDateID, repo.gotDeadlineEndDateTenant, deadlineID, tenantID)
	}
	if repo.updateTaskCalls != 1 || len(publishedOfType[TaskUpdated](outbox)) != 1 {
		t.Errorf("UpdateTask/published = %d/%d, want 1/1", repo.updateTaskCalls, len(publishedOfType[TaskUpdated](outbox)))
	}
}

// TestUpdateTask_DueDateAfterDeadlineEnd rejects a PATCH that moves the task's due_date past its
// prazo's end_date: nothing updates, nothing emits, with a typed KindInvalid (→ 400).
func TestUpdateTask_DueDateAfterDeadlineEnd(t *testing.T) {
	deadlineID := uuid.NewString()
	end := time.Date(2024, 3, 20, 0, 0, 0, 0, time.UTC)
	due := time.Date(2024, 3, 25, 0, 0, 0, 0, time.UTC)

	repo := &mockRepo{
		taskForUpdate: &TaskForUpdate{
			ID: uuid.NewString(), Status: TaskStatusOpen, Title: "antigo", Kind: "PECA", DeadlineID: deadlineID,
		},
		deadlineEndDate: end,
	}
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, &fakeCalendar{}, outbox, &fakeDedup{}, &fakeUOW{})

	dueWire := due.Format("2006-01-02")
	_, err := uc.UpdateTask(context.Background(), UpdateTaskCommand{
		TenantID: uuid.NewString(), TaskID: uuid.NewString(), DueDate: &dueWire,
	})
	ae, ok := apperr.From(err)
	if !ok || ae.Kind != apperr.KindInvalid {
		t.Errorf("error = %v, want KindInvalid", err)
	}
	if repo.updateTaskCalls != 0 || len(outbox.published) != 0 {
		t.Errorf("update/published = %d/%d, want 0/0", repo.updateTaskCalls, len(outbox.published))
	}
}

// --- MarkTaskDone / DismissTask ---------------------------------------------

// TestMarkTaskDone_OpenToDone is the happy path for concluir: a still-OPEN task flips to DONE
// (guarded OPEN→DONE) stamping completed_at=now, and exactly one task.completed is emitted with
// the task id as a parseable uuid aggregate.
func TestMarkTaskDone_OpenToDone(t *testing.T) {
	tenantID := uuid.NewString()
	taskID := uuid.NewString()
	now := time.Date(2024, 3, 20, 9, 30, 0, 0, time.UTC)
	repo := &mockRepo{taskTransition: TaskStatusOpen, markTaskStatusID: taskID}
	outbox := &fakeOutbox{}
	uow := &fakeUOW{}
	uc := NewUseCase(repo, &fakeCalendar{}, outbox, &fakeDedup{}, uow, WithClock(func() time.Time { return now }))

	res, err := uc.MarkTaskDone(context.Background(), tenantID, uuid.NewString(), taskID)
	if err != nil {
		t.Fatalf("MarkTaskDone() error = %v", err)
	}
	if res.ID != taskID || res.Status != TaskStatusDone {
		t.Errorf("result = %+v, want %q/DONE", res, taskID)
	}
	if len(uow.scopes) != 1 || uow.scopes[0] != tenantID {
		t.Errorf("uow scopes = %v, want [%q]", uow.scopes, tenantID)
	}
	// The flip was guarded OPEN→DONE, tenant-scoped, stamping completed_at=now.
	if repo.markTaskStatusCalls != 1 || repo.gotMarkTaskFrom != TaskStatusOpen || repo.gotMarkTaskTo != TaskStatusDone {
		t.Errorf("MarkTaskStatus calls/from/to = %d/%q/%q, want 1/OPEN/DONE", repo.markTaskStatusCalls, repo.gotMarkTaskFrom, repo.gotMarkTaskTo)
	}
	if repo.gotMarkTaskID != taskID || repo.gotMarkTaskTenantID != tenantID {
		t.Errorf("flip id/tenant = %q/%q, want %q/%q", repo.gotMarkTaskID, repo.gotMarkTaskTenantID, taskID, tenantID)
	}
	if repo.gotMarkTaskCompleted == nil || !repo.gotMarkTaskCompleted.Equal(now) {
		t.Errorf("completed_at = %v, want %v (now)", repo.gotMarkTaskCompleted, now)
	}

	completed := publishedOfType[TaskCompleted](outbox)
	if len(completed) != 1 {
		t.Fatalf("task.completed events = %d, want 1", len(completed))
	}
	c := completed[0]
	if c.Type() != TypeTaskCompleted || c.AggregateType() != aggregateTypeTask || c.AggregateID() != taskID || c.TaskID != taskID {
		t.Errorf("task.completed type/aggregate/task = %q/%q/%q/%q", c.Type(), c.AggregateType(), c.AggregateID(), c.TaskID)
	}
}

// TestDismissTask_OpenToDismissed is the manual dispensar: a still-OPEN task flips to DISMISSED
// WITHOUT stamping completed_at (dispensar is not a completion), emitting one task.dismissed.
func TestDismissTask_OpenToDismissed(t *testing.T) {
	tenantID := uuid.NewString()
	taskID := uuid.NewString()
	repo := &mockRepo{taskTransition: TaskStatusOpen, markTaskStatusID: taskID}
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, &fakeCalendar{}, outbox, &fakeDedup{}, &fakeUOW{})

	res, err := uc.DismissTask(context.Background(), tenantID, uuid.NewString(), taskID)
	if err != nil {
		t.Fatalf("DismissTask() error = %v", err)
	}
	if res.Status != TaskStatusDismissed {
		t.Errorf("result status = %q, want DISMISSED", res.Status)
	}
	if repo.gotMarkTaskFrom != TaskStatusOpen || repo.gotMarkTaskTo != TaskStatusDismissed {
		t.Errorf("flip from/to = %q/%q, want OPEN/DISMISSED", repo.gotMarkTaskFrom, repo.gotMarkTaskTo)
	}
	if repo.gotMarkTaskCompleted != nil {
		t.Errorf("completed_at = %v, want nil (dispensar is not a completion)", repo.gotMarkTaskCompleted)
	}
	dismissed := publishedOfType[TaskDismissed](outbox)
	if len(dismissed) != 1 || dismissed[0].TaskID != taskID {
		t.Errorf("task.dismissed events = %d (task %v), want 1 for %q", len(dismissed), dismissed, taskID)
	}
}

// TestTask_TransitionRequiresOpen proves both manual transitions are OPEN-only: from any non-OPEN
// status they return ErrTaskNotOpen (→ 409) without flipping or emitting.
func TestTask_TransitionRequiresOpen(t *testing.T) {
	transitions := []struct {
		name string
		call func(uc *UseCase, tenantID, taskID string) (TaskTransition, error)
	}{
		{"done", func(uc *UseCase, tenantID, taskID string) (TaskTransition, error) {
			return uc.MarkTaskDone(context.Background(), tenantID, uuid.NewString(), taskID)
		}},
		{"dismiss", func(uc *UseCase, tenantID, taskID string) (TaskTransition, error) {
			return uc.DismissTask(context.Background(), tenantID, uuid.NewString(), taskID)
		}},
	}
	for _, tr := range transitions {
		for _, status := range []TaskStatus{TaskStatusDone, TaskStatusDismissed} {
			t.Run(tr.name+"/"+string(status), func(t *testing.T) {
				repo := &mockRepo{taskTransition: status}
				outbox := &fakeOutbox{}
				uc := NewUseCase(repo, &fakeCalendar{}, outbox, &fakeDedup{}, &fakeUOW{})

				_, err := tr.call(uc, uuid.NewString(), uuid.NewString())
				ae, ok := apperr.From(err)
				if !ok || ae.Kind != apperr.KindConflict {
					t.Errorf("error = %v, want KindConflict (not open)", err)
				}
				if repo.markTaskStatusCalls != 0 || len(outbox.published) != 0 {
					t.Errorf("flip/published ran on a non-OPEN task: %d/%d", repo.markTaskStatusCalls, len(outbox.published))
				}
			})
		}
	}
}

// TestTask_TransitionNotFound proves transitioning an unknown/foreign task is the typed
// ErrTaskNotFound (→ 404): the status re-read misses, so nothing is flipped or emitted.
func TestTask_TransitionNotFound(t *testing.T) {
	repo := &mockRepo{taskTransitionErr: ErrTaskNotFound}
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, &fakeCalendar{}, outbox, &fakeDedup{}, &fakeUOW{})

	_, err := uc.MarkTaskDone(context.Background(), uuid.NewString(), uuid.NewString(), uuid.NewString())
	ae, ok := apperr.From(err)
	if !ok || ae.Kind != apperr.KindNotFound {
		t.Errorf("error = %v, want KindNotFound", err)
	}
	if repo.markTaskStatusCalls != 0 || len(outbox.published) != 0 {
		t.Errorf("flip/published = %d/%d, want 0/0 on not-found", repo.markTaskStatusCalls, len(outbox.published))
	}
}

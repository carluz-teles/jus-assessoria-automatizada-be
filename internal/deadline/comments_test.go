package deadline

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jusassessoria/platform/lib/apperr"
)

// --- priority (BE-1) --------------------------------------------------------

// TestCreateTask_CarriesPriorityAndRecordsCreatedActivity proves POST /v1/tasks persists the
// priority flag on the task and appends exactly one TASK_CREATED activity row (from/to NULL) in
// the same tx.
func TestCreateTask_CarriesPriorityAndRecordsCreatedActivity(t *testing.T) {
	tenantID := uuid.NewString()
	userID := uuid.NewString()
	repo := &mockRepo{}
	uc := NewUseCase(repo, &fakeCalendar{}, &fakeOutbox{}, &fakeDedup{}, &fakeUOW{})

	_, err := uc.CreateTask(context.Background(), CreateTaskCommand{
		TenantID: tenantID, UserID: userID,
		Title: "Peça", Priority: string(TaskPriorityHigh),
	})
	if err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}

	if len(repo.insertedTasks) != 1 || repo.insertedTasks[0].Priority != string(TaskPriorityHigh) {
		t.Fatalf("inserted task priority = %q, want HIGH", repo.insertedTasks[0].Priority)
	}
	if len(repo.insertedActivities) != 1 {
		t.Fatalf("activity rows = %d, want 1 (TASK_CREATED)", len(repo.insertedActivities))
	}
	a := repo.insertedActivities[0]
	if a.EventType != ActivityTaskCreated || a.ActorUserID != userID || a.FromValue != "" || a.ToValue != "" {
		t.Errorf("activity = %+v, want TASK_CREATED by %q with empty from/to", a, userID)
	}
}

// TestUpdateTask_RecordsActivityOnlyForChangedFields proves PATCH /v1/tasks/:id appends one
// activity row per field that ACTUALLY changed (priority + due_date here), and none for a field
// patched to its stored value (title unchanged). The de/para is captured on each row.
func TestUpdateTask_RecordsActivityOnlyForChangedFields(t *testing.T) {
	tenantID := uuid.NewString()
	userID := uuid.NewString()
	taskID := uuid.NewString()
	oldDue := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	newDue := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)

	repo := &mockRepo{
		taskForUpdate: &TaskForUpdate{
			ID: taskID, Status: TaskStatusOpen, Title: "Peça", Priority: string(TaskPriorityLow), DueDate: &oldDue,
		},
		updatedTask: &Task{
			ID: taskID, TenantID: tenantID, Title: "Peça", Priority: string(TaskPriorityHigh), DueDate: &newDue, Status: TaskStatusOpen, Source: SourceManual,
		},
	}
	uc := NewUseCase(repo, &fakeCalendar{}, &fakeOutbox{}, &fakeDedup{}, &fakeUOW{})

	title := "Peça"
	prio := string(TaskPriorityHigh)
	dueWire := newDue.Format(time.DateOnly)
	_, err := uc.UpdateTask(context.Background(), UpdateTaskCommand{
		TenantID: tenantID, UserID: userID, TaskID: taskID,
		Title: &title, Priority: &prio, DueDate: &dueWire,
	})
	if err != nil {
		t.Fatalf("UpdateTask() error = %v", err)
	}

	// The merged params carry the new priority through to the repo write.
	if repo.gotUpdateTaskParams.Priority != string(TaskPriorityHigh) {
		t.Errorf("update params priority = %q, want HIGH", repo.gotUpdateTaskParams.Priority)
	}

	// Two rows: PRIORITY_CHANGED and DUE_DATE_CHANGED; title was patched to its stored value → none.
	got := map[ActivityEventType][2]string{}
	for _, a := range repo.insertedActivities {
		got[a.EventType] = [2]string{a.FromValue, a.ToValue}
	}
	if len(repo.insertedActivities) != 2 {
		t.Fatalf("activity rows = %d, want 2 (priority + due_date), got %+v", len(repo.insertedActivities), repo.insertedActivities)
	}
	if got[ActivityPriorityChanged] != [2]string{"LOW", "HIGH"} {
		t.Errorf("PRIORITY_CHANGED from/to = %v, want [LOW HIGH]", got[ActivityPriorityChanged])
	}
	if got[ActivityDueDateChanged] != [2]string{"2026-01-10", "2026-01-15"} {
		t.Errorf("DUE_DATE_CHANGED from/to = %v, want [2026-01-10 2026-01-15]", got[ActivityDueDateChanged])
	}
	if _, ok := got[ActivityTitleChanged]; ok {
		t.Error("TITLE_CHANGED recorded for an unchanged title")
	}
}

// TestMarkTaskDone_RecordsDoneActivity proves the done transition appends a TASK_DONE activity row
// with the actor, in the same tx as the flip.
func TestMarkTaskDone_RecordsDoneActivity(t *testing.T) {
	tenantID := uuid.NewString()
	userID := uuid.NewString()
	taskID := uuid.NewString()
	repo := &mockRepo{taskTransition: TaskStatusOpen, markTaskStatusID: taskID}
	uc := NewUseCase(repo, &fakeCalendar{}, &fakeOutbox{}, &fakeDedup{}, &fakeUOW{})

	if _, err := uc.MarkTaskDone(context.Background(), tenantID, userID, taskID); err != nil {
		t.Fatalf("MarkTaskDone() error = %v", err)
	}
	if len(repo.insertedActivities) != 1 || repo.insertedActivities[0].EventType != ActivityTaskDone {
		t.Fatalf("activity = %+v, want one TASK_DONE", repo.insertedActivities)
	}
	if repo.insertedActivities[0].ActorUserID != userID {
		t.Errorf("actor = %q, want %q", repo.insertedActivities[0].ActorUserID, userID)
	}
}

// --- comments (BE-2/BE-3) ---------------------------------------------------

// TestCreateTaskComment_GuardsParentInsertsAndRecordsActivity is the happy path for POST
// /v1/tasks/:id/comments: it guards the parent task exists, persists the comment authored by the
// principal, and appends a COMMENTED activity row — all in one tenant-scoped tx.
func TestCreateTaskComment_GuardsParentInsertsAndRecordsActivity(t *testing.T) {
	tenantID := uuid.NewString()
	userID := uuid.NewString()
	taskID := uuid.NewString()
	repo := &mockRepo{}
	uow := &fakeUOW{}
	uc := NewUseCase(repo, &fakeCalendar{}, &fakeOutbox{}, &fakeDedup{}, uow)

	comment, err := uc.CreateTaskComment(context.Background(), CreateTaskCommentCommand{
		TenantID: tenantID, UserID: userID, TaskID: taskID, Body: "Já protocolei",
	})
	if err != nil {
		t.Fatalf("CreateTaskComment() error = %v", err)
	}
	if comment.ID == "" || comment.Body != "Já protocolei" || comment.AuthorUserID != userID {
		t.Errorf("comment = %+v, want id/body/author set", comment)
	}
	if len(uow.scopes) != 1 || uow.scopes[0] != tenantID {
		t.Errorf("uow scopes = %v, want [%q]", uow.scopes, tenantID)
	}
	if len(repo.insertedActivities) != 1 || repo.insertedActivities[0].EventType != ActivityCommented {
		t.Fatalf("activity = %+v, want one COMMENTED", repo.insertedActivities)
	}
}

// TestCreateTaskComment_ParentMissIsNotFound proves a comment on a foreign/unknown task is the
// typed ErrTaskNotFound (→ 404): the parent guard fails, so nothing is inserted.
func TestCreateTaskComment_ParentMissIsNotFound(t *testing.T) {
	repo := &mockRepo{ensureTaskExistsErr: ErrTaskNotFound}
	uc := NewUseCase(repo, &fakeCalendar{}, &fakeOutbox{}, &fakeDedup{}, &fakeUOW{})

	_, err := uc.CreateTaskComment(context.Background(), CreateTaskCommentCommand{
		TenantID: uuid.NewString(), UserID: uuid.NewString(), TaskID: uuid.NewString(), Body: "x",
	})
	ae, ok := apperr.From(err)
	if !ok || ae.Kind != apperr.KindNotFound {
		t.Errorf("error = %v, want KindNotFound", err)
	}
	if len(repo.insertedActivities) != 0 {
		t.Error("activity recorded despite the parent-task miss")
	}
}

package deadline

import (
	"context"
	"fmt"
	"time"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// tasks.go is the task WRITE path — the task CRUD surface of the slice (docs/erd-prazos.md §9:
// POST /v1/tasks, PATCH /v1/tasks/:id, POST /v1/tasks/:id/done | .../dismiss). It is the
// sibling of confirm.go/adjust.go: it REUSES the same uow/outbox transactional-outbox pattern,
// the same typed lib/apperr errors, the same guarded-UPDATE idiom for lifecycle transitions
// (mirroring markStatus), and the InsertTask repo write the F2 confirm already uses — it never
// forks a parallel persistence path nor a parallel event contract.

// CreateTaskCommand is the manual CREATE input the handler builds from the request + the
// verified principal (docs §9, POST /v1/tasks). TenantID and UserID come from the principal,
// NEVER the body (tenant isolation cannot be spoofed); the context FKs (court_record_id,
// deadline_id, intimation_id) are optional — a task can be avulsa. The task is born OPEN,
// source MANUAL, created_by the principal.
type CreateTaskCommand struct {
	TenantID       string
	UserID         string
	CourtRecordID  string
	DeadlineID     string
	IntimationID   string
	Title          string
	Description    string
	Kind           string
	DueDate        *time.Time
	AssigneeUserID string
}

// UpdateTaskCommand is the ajuste input the handler builds from the request + the verified
// principal (docs §9, PATCH /v1/tasks/:id). TenantID comes from the principal and TaskID from
// the path (never the body). Every editable field is a POINTER so "present in the body" is
// distinguishable from "absent": a nil field keeps the task's stored value, a non-nil one
// overrides it (a partial patch). DueDate is the wire date ("" clears it, an absent field keeps
// it) — the two must not collapse. The edit never touches status/source/created_by.
type UpdateTaskCommand struct {
	TenantID       string
	TaskID         string
	Title          *string
	Description    *string
	Kind           *string
	DueDate        *string
	AssigneeUserID *string
}

// TaskForUpdate is the editable state PATCH /v1/tasks/:id loads BEFORE the merge (GetTaskForUpdate),
// keyed by id: the current {title, description, kind, due_date, assignee} the partial patch is
// applied over (an absent field keeps its stored value), plus the Status carried for the caller
// (the edit is orthogonal to the lifecycle — it never changes it). A missing task is
// ErrTaskNotFound (→ 404), never a zero value.
type TaskForUpdate struct {
	ID             string
	Status         TaskStatus
	Title          string
	Description    string
	Kind           string
	DueDate        *time.Time
	AssigneeUserID string
	DeadlineID     string
}

// UpdateTaskParams is the repo port's input for the UpdateTask UPDATE — the merged editable
// fields (already merged over the stored values by the use case), keyed by task id + tenant. It
// is a plain struct (not the Task aggregate) because the edit updates a subset of columns.
type UpdateTaskParams struct {
	TaskID         string
	TenantID       string
	Title          string
	Description    string
	Kind           string
	DueDate        *time.Time
	AssigneeUserID string
}

// TaskTransition is the outcome of a manual transition (done/dismiss): the task id and its new
// status. The handler renders it as the response; the use case has already emitted the matching
// task.completed / task.dismissed event in the same tx. It mirrors MarkedDeadline.
type TaskTransition struct {
	ID     string
	Status TaskStatus
}

// CreateTask is the manual "Criar tarefa" (docs/erd-prazos.md §9, POST /v1/tasks): in ONE
// tenant-scoped tx it persists a task born OPEN/MANUAL/created_by=principal and emits
// task.created — the entity write and the outbox row committing together (transactional
// outbox). It REUSES the InsertTask the F2 confirm uses, so a manual and a confirmed task are
// the same row shape. TenantID/UserID come from the principal (never the body).
func (uc *UseCase) CreateTask(ctx context.Context, cmd CreateTaskCommand) (*Task, error) {
	var created *Task
	err := uc.uow.Do(ctx, cmd.TenantID, func(tx database.Tx) error {
		if err := uc.validateTaskDueDate(ctx, tx, cmd.DeadlineID, cmd.TenantID, cmd.DueDate); err != nil {
			return err
		}
		saved, err := uc.repo.InsertTask(ctx, tx, &Task{
			TenantID:       cmd.TenantID,
			CourtRecordID:  cmd.CourtRecordID,
			DeadlineID:     cmd.DeadlineID,
			IntimationID:   cmd.IntimationID,
			Title:          cmd.Title,
			Description:    cmd.Description,
			Kind:           cmd.Kind,
			DueDate:        cmd.DueDate,
			Status:         TaskStatusOpen,
			Source:         SourceManual,
			AssigneeUserID: cmd.AssigneeUserID,
			CreatedBy:      cmd.UserID,
		})
		if err != nil {
			return err
		}
		if err := uc.outbox.Publish(ctx, tx, newTaskCreatedFromTask(saved)); err != nil {
			return err
		}
		created = saved
		return nil
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

// UpdateTask is the manual ajuste (docs/erd-prazos.md §9, PATCH /v1/tasks/:id): in ONE
// tenant-scoped tx it merges the partial patch over the task's stored editable fields, UPDATEs
// the row, and emits task.updated. The edit is deliberately NOT gated on status (editing a
// DONE/DISMISSED task's title/assignee is harmless and never changes the lifecycle — the
// OPEN→DONE/DISMISSED transitions live only in MarkDone/Dismiss). A missing task is the repo's
// typed ErrTaskNotFound (→ 404). TenantID comes from the principal, the id from the path.
func (uc *UseCase) UpdateTask(ctx context.Context, cmd UpdateTaskCommand) (*Task, error) {
	var updated *Task
	err := uc.uow.Do(ctx, cmd.TenantID, func(tx database.Tx) error {
		cur, err := uc.repo.GetTaskForUpdate(ctx, tx, cmd.TaskID, cmd.TenantID)
		if err != nil {
			return err
		}

		// Merge the partial patch over the stored values: an absent (nil) field keeps the task's
		// current value, a present one overrides it. DueDate is the wire date — a present "" clears
		// the date, a present value sets it, an absent field keeps the stored date.
		title := cur.Title
		if cmd.Title != nil {
			title = *cmd.Title
		}
		description := cur.Description
		if cmd.Description != nil {
			description = *cmd.Description
		}
		kind := cur.Kind
		if cmd.Kind != nil {
			kind = *cmd.Kind
		}
		assignee := cur.AssigneeUserID
		if cmd.AssigneeUserID != nil {
			assignee = *cmd.AssigneeUserID
		}
		dueDate := cur.DueDate
		if cmd.DueDate != nil {
			dueDate = parseOptionalWireDate(*cmd.DueDate) // "" → nil (clear), a wire date → set
		}

		if err := uc.validateTaskDueDate(ctx, tx, cur.DeadlineID, cmd.TenantID, dueDate); err != nil {
			return err
		}

		saved, err := uc.repo.UpdateTask(ctx, tx, UpdateTaskParams{
			TaskID:         cmd.TaskID,
			TenantID:       cmd.TenantID,
			Title:          title,
			Description:    description,
			Kind:           kind,
			DueDate:        dueDate,
			AssigneeUserID: assignee,
		})
		if err != nil {
			return err
		}
		if err := uc.outbox.Publish(ctx, tx, newTaskUpdated(saved.ID)); err != nil {
			return err
		}
		updated = saved
		return nil
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// MarkTaskDone is the manual "concluir tarefa" (docs/erd-prazos.md §9, POST /v1/tasks/:id/done):
// OPEN→DONE in ONE tenant-scoped tx, stamping completed_at and emitting task.completed. It is
// the positive counterpart of DismissTask and shares its shape.
func (uc *UseCase) MarkTaskDone(ctx context.Context, tenantID, taskID string) (TaskTransition, error) {
	return uc.markTaskStatus(ctx, tenantID, taskID, TaskStatusDone)
}

// DismissTask is the manual "dispensar tarefa" (docs/erd-prazos.md §9, POST /v1/tasks/:id/dismiss):
// OPEN→DISMISSED in ONE tenant-scoped tx, emitting task.dismissed. completed_at is left NULL —
// dispensar is not a completion, so only DONE stamps it.
func (uc *UseCase) DismissTask(ctx context.Context, tenantID, taskID string) (TaskTransition, error) {
	return uc.markTaskStatus(ctx, tenantID, taskID, TaskStatusDismissed)
}

// markTaskStatus is the shared manual-transition path behind MarkTaskDone/DismissTask: both flip
// a task OPEN→<target> and emit the matching immediate fact. The guard is intentionally strict —
// only OPEN transitions (a terminal DONE/DISMISSED task cannot transition again), mirroring the
// deadline markStatus:
//  1. re-read the task's status (a miss → ErrTaskNotFound → 404);
//  2. it must be OPEN, else ErrTaskNotOpen (→ 409, distinct from the 404 miss);
//  3. MarkTaskStatus OPEN→target (the `status = OPEN` guard is the concurrency floor); DONE
//     stamps completed_at=now, DISMISSED leaves it NULL;
//  4. emit the target's immediate fact (task.completed / task.dismissed) in the SAME tx.
//
// TenantID comes from the verified principal and scopes the tx's RLS (barrier 1 + 2).
func (uc *UseCase) markTaskStatus(ctx context.Context, tenantID, taskID string, target TaskStatus) (TaskTransition, error) {
	var result TaskTransition
	err := uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		status, err := uc.repo.GetTaskForTransition(ctx, tx, taskID, tenantID)
		if err != nil {
			return err
		}
		if status != TaskStatusOpen {
			return ErrTaskNotOpen
		}

		// completed_at is stamped only on DONE (a completion); DISMISSED leaves it NULL.
		var completedAt *time.Time
		if target == TaskStatusDone {
			now := uc.now()
			completedAt = &now
		}

		id, err := uc.repo.MarkTaskStatus(ctx, tx, taskID, tenantID, TaskStatusOpen, target, completedAt)
		if err != nil {
			return err
		}

		if err := uc.outbox.Publish(ctx, tx, newTaskTransitionEvent(target, id)); err != nil {
			return err
		}
		result = TaskTransition{ID: id, Status: target}
		return nil
	})
	if err != nil {
		return TaskTransition{}, err
	}
	return result, nil
}

// newTaskTransitionEvent picks the immediate fact for a manual task transition: task.completed
// for DONE, task.dismissed for DISMISSED. Only these two targets reach here (markTaskStatus is
// the sole caller, with the two Mark* wrappers), so the default is defensive.
func newTaskTransitionEvent(target TaskStatus, taskID string) events.Event {
	if target == TaskStatusDone {
		return newTaskCompleted(taskID)
	}
	return newTaskDismissed(taskID)
}

// validateTaskDueDate enforces ERD §4's task invariant: a task linked to a prazo (DeadlineID)
// cannot carry a due_date after that prazo's end_date. A nil dueDate (avulsa/undated task, or a
// PATCH that clears the date) is a no-op. deadlineID empty means an avulsa task — no prazo bound,
// so no check. A violation is a typed KindInvalid (→ 400), reported with the offending
// values so the FE can point at the field. It runs INSIDE the caller's tx (the task write path
// validates before persisting) and REUSES the narrow GetDeadlineEndDate read (a missing prazo is
// the repo's typed ErrDeadlineNotFound — a dangling deadline_id is a hard fault, never a silent
// acceptance).
func (uc *UseCase) validateTaskDueDate(ctx context.Context, tx database.Tx, deadlineID, tenantID string, dueDate *time.Time) error {
	if deadlineID == "" || dueDate == nil {
		return nil
	}
	endDate, err := uc.repo.GetDeadlineEndDate(ctx, tx, deadlineID, tenantID)
	if err != nil {
		return err
	}
	if dueDate.After(endDate) {
		return apperr.NewInvalid(fmt.Sprintf("task due_date %s is after the deadline end_date %s",
			dueDate.Format(time.DateOnly), endDate.Format(time.DateOnly)))
	}
	return nil
}

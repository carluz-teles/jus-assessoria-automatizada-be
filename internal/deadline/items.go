package deadline

import (
	"context"
	"time"

	"github.com/jusassessoria/platform/lib/database"
)

// items.go is the task-item (checklist / subtarefas) WRITE path — the CRUD surface behind the
// Tarefas screen's checklist (docs/erd-prazos.md §4/§10: POST /v1/tasks/:id/items, PATCH
// …/items/:itemId, DELETE …/items/:itemId). It is a sibling of tasks.go: it REUSES the same
// uow tenant-scoped tx (barrier 1 + RLS barrier 2), the same typed lib/apperr errors, and the
// same GetForUpdate → merge → UPDATE idiom the task PATCH uses. Items carry NO domain event
// (they are UI checklist state, not a business fact the outbox announces) — the tx is the only
// consistency boundary. Idempotency is not critical here (the FE owns the checklist state).

// CreateTaskItemCommand appends one checklist item to a task (POST /v1/tasks/:id/items).
// TenantID comes from the principal and TaskID from the path (never the body); Title is the
// only user input. Position is server-computed (appended last), and the item is born
// done=false.
type CreateTaskItemCommand struct {
	TenantID string
	TaskID   string
	Title    string
}

// UpdateTaskItemCommand is the partial PATCH of a checklist item (PATCH …/items/:itemId).
// TenantID/TaskID/ItemID come from the principal + path. Done and Title are POINTERS so an
// ABSENT field keeps its stored value (a partial patch): a nil Done keeps the tick, a nil
// Title keeps the label. done_at is derived from Done by the use case, never accepted.
type UpdateTaskItemCommand struct {
	TenantID string
	TaskID   string
	ItemID   string
	Title    *string
	Done     *bool
}

// TaskItemForUpdate is the editable state PATCH loads BEFORE the merge (GetTaskItemForUpdate):
// the current {title, done} the partial patch is applied over. A missing item under the parent
// task is ErrTaskItemNotFound (→ 404), never a zero value.
type TaskItemForUpdate struct {
	ID     string
	TaskID string
	Title  string
	Done   bool
}

// UpdateTaskItemParams is the repo port's input for the UPDATE — the merged {title, done} plus
// the DoneAt the use case derived from done (a real time when done is true, nil otherwise),
// keyed by (item, task, tenant).
type UpdateTaskItemParams struct {
	ItemID   string
	TaskID   string
	TenantID string
	Title    string
	Done     bool
	DoneAt   *time.Time
}

// CreateTaskItem appends a checklist item to a task in ONE tenant-scoped tx (docs §4/§10). It
// first guards the parent task exists in the tenant (a foreign/unknown :id is ErrTaskItemNotFound
// → 404, never an item grafted onto nothing), then computes the append position and inserts the
// item born done=false. TenantID/TaskID come from the principal + path.
func (uc *UseCase) CreateTaskItem(ctx context.Context, cmd CreateTaskItemCommand) (*TaskItem, error) {
	var created *TaskItem
	err := uc.uow.Do(ctx, cmd.TenantID, func(tx database.Tx) error {
		if err := uc.repo.EnsureTaskInTenant(ctx, tx, cmd.TaskID, cmd.TenantID); err != nil {
			return err
		}
		position, err := uc.repo.NextTaskItemPosition(ctx, tx, cmd.TaskID, cmd.TenantID)
		if err != nil {
			return err
		}
		saved, err := uc.repo.InsertTaskItem(ctx, tx, &TaskItem{
			TenantID: cmd.TenantID,
			TaskID:   cmd.TaskID,
			Title:    cmd.Title,
			Position: position,
		})
		if err != nil {
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

// UpdateTaskItem is the partial PATCH of a checklist item in ONE tenant-scoped tx (toggle done
// and/or rename). It merges the present fields over the stored {title, done} (an absent field
// keeps its value), then derives done_at from the RESULTING done — stamping now when it becomes
// done and clearing it when it becomes undone (so re-opening a step drops its completion time).
// A missing item under the parent task is the repo's typed ErrTaskItemNotFound (→ 404).
func (uc *UseCase) UpdateTaskItem(ctx context.Context, cmd UpdateTaskItemCommand) (*TaskItem, error) {
	var updated *TaskItem
	err := uc.uow.Do(ctx, cmd.TenantID, func(tx database.Tx) error {
		cur, err := uc.repo.GetTaskItemForUpdate(ctx, tx, cmd.ItemID, cmd.TaskID, cmd.TenantID)
		if err != nil {
			return err
		}

		title := cur.Title
		if cmd.Title != nil {
			title = *cmd.Title
		}
		done := cur.Done
		if cmd.Done != nil {
			done = *cmd.Done
		}

		// done_at tracks done: stamp now when the step becomes done, clear it when it is undone.
		var doneAt *time.Time
		if done {
			now := uc.now()
			doneAt = &now
		}

		saved, err := uc.repo.UpdateTaskItem(ctx, tx, UpdateTaskItemParams{
			ItemID:   cmd.ItemID,
			TaskID:   cmd.TaskID,
			TenantID: cmd.TenantID,
			Title:    title,
			Done:     done,
			DoneAt:   doneAt,
		})
		if err != nil {
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

// DeleteTaskItem removes one checklist item in ONE tenant-scoped tx, keyed by (item, task,
// tenant). A no-match (foreign/unknown item, or one under a different task) is the repo's typed
// ErrTaskItemNotFound (→ 404), never a silent success on nothing.
func (uc *UseCase) DeleteTaskItem(ctx context.Context, tenantID, taskID, itemID string) error {
	return uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		return uc.repo.DeleteTaskItem(ctx, tx, itemID, taskID, tenantID)
	})
}

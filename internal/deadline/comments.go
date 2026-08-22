package deadline

import (
	"context"

	"github.com/jusassessoria/platform/lib/database"
)

// comments.go is the task-comment (discussion thread) WRITE path plus the shared task_activity
// (audit log) helper (docs/erd-prazos.md §4/§10: the Tarefa detail's Comentários and Atividade
// tabs). It is a sibling of items.go: it REUSES the same uow tenant-scoped tx (barrier 1 + RLS
// barrier 2), the same typed lib/apperr errors, and the same parent-task guard idiom the checklist
// uses. A comment carries NO domain event (it is UI thread state, not a business fact the outbox
// announces) — the tx is the only consistency boundary. Activity rows are written by the task
// mutation paths (tasks.go) via recordActivity, always in the mutation's own tx so the audit log
// never diverges from the task's real history.

// CreateTaskCommentCommand appends one comment to a task's thread (POST /v1/tasks/:id/comments).
// TenantID/UserID come from the verified principal (never the body) and TaskID from the path; Body
// is the only user input.
type CreateTaskCommentCommand struct {
	TenantID string
	UserID   string
	TaskID   string
	Body     string
}

// CreateTaskComment appends a comment to a task in ONE tenant-scoped tx (docs §4/§10). It first
// guards the parent task exists in the tenant (a foreign/unknown :id is ErrTaskNotFound → 404,
// never a comment grafted onto nothing), then inserts the comment authored by the principal and
// records a COMMENTED activity row in the SAME tx (the Atividade tab shows a comment as an event).
// TenantID/UserID come from the principal, TaskID from the path.
func (uc *UseCase) CreateTaskComment(ctx context.Context, cmd CreateTaskCommentCommand) (*TaskComment, error) {
	var created *TaskComment
	err := uc.uow.Do(ctx, cmd.TenantID, func(tx database.Tx) error {
		if err := uc.repo.EnsureTaskExistsInTenant(ctx, tx, cmd.TaskID, cmd.TenantID); err != nil {
			return err
		}
		saved, err := uc.repo.InsertTaskComment(ctx, tx, &TaskComment{
			TenantID:     cmd.TenantID,
			TaskID:       cmd.TaskID,
			AuthorUserID: cmd.UserID,
			Body:         cmd.Body,
		})
		if err != nil {
			return err
		}
		// COMMENTED activity, same tx as the comment insert. from/to are NULL — a comment is not a
		// field change.
		if err := uc.recordActivity(ctx, tx, cmd.TenantID, cmd.TaskID, cmd.UserID, ActivityCommented, nil, nil); err != nil {
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

// recordActivity appends ONE task_activity row inside the caller's tx — the shared audit-log
// write every task mutation path calls (create, per-field edit, done/dismiss, comment). It runs in
// the mutation's OWN tx so the log commits atomically with the change it records (a rolled-back
// mutation leaves no phantom log row). from/to are the field change's before/after (both nil for a
// create/lifecycle/comment); they are pre-normalized to nil by the caller when empty.
func (uc *UseCase) recordActivity(ctx context.Context, tx database.Tx, tenantID, taskID, actorID string, event ActivityEventType, from, to *string) error {
	fromValue := ""
	if from != nil {
		fromValue = *from
	}
	toValue := ""
	if to != nil {
		toValue = *to
	}
	return uc.repo.InsertTaskActivity(ctx, tx, &TaskActivity{
		TenantID:    tenantID,
		TaskID:      taskID,
		ActorUserID: actorID,
		EventType:   event,
		FromValue:   fromValue,
		ToValue:     toValue,
	})
}

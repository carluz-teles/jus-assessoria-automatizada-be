package deadline

import (
	"context"
	"errors"

	"github.com/jusassessoria/platform/internal/actionitem"
	"github.com/jusassessoria/platform/lib/database"
)

// actionitem_task.go implements the SYNCHRONOUS half of the providência → tarefa loop
// (docs/erd-costura-providencia-tarefa-peca.md §2/§6, revisão síncrona): internal/actionitem
// creates the task INSIDE its own request/worker tx by calling actionitem.TaskCreator, and this
// slice is the impl injected there (WithTaskCreator). The old async path (consume
// actionitem.created/confirmed → InsertTask → emit task.created → actionitem consumes it to link)
// is gone — the event hop fell into a queue nobody drained, so the task_id never got linked and a
// user-driven confirm could stall behind backlog. Now the task write + the reverse-pointer link
// commit in ONE tx, no fila.
//
// It REUSES tasks.go's InsertTask write shape (born OPEN/RULE, task.action_item_id set), so an
// automatic task and a manual one are the same row.

// taskCreatorRepo is the narrow subset of the deadline Repository this adapter needs — the same
// *pgRepository the rest of the slice uses. Kept as a local interface so the adapter can be unit-
// tested with a fake without dragging the whole Repository surface in.
type taskCreatorRepo interface {
	InsertTask(ctx context.Context, tx database.Tx, t *Task) (*Task, error)
	GetTaskIDByActionItem(ctx context.Context, tx database.Tx, tenantID, actionItemID string) (string, error)
}

// ActionItemTaskCreator adapts the deadline slice to actionitem.TaskCreator: it mints (or finds)
// the tarefa of a confiável providência inside the actionitem use case's tx. It is stateless save
// the repo it delegates to; the api/worker composition constructs one and passes it via
// actionitem.WithTaskCreator.
type ActionItemTaskCreator struct {
	repo taskCreatorRepo
}

// Compile-time proof the adapter satisfies the port defined in internal/actionitem.
var _ actionitem.TaskCreator = (*ActionItemTaskCreator)(nil)

// NewActionItemTaskCreator wires the adapter to a Repository (NewRepository() in production).
func NewActionItemTaskCreator(repo taskCreatorRepo) *ActionItemTaskCreator {
	return &ActionItemTaskCreator{repo: repo}
}

// CreateForActionItem persists the task for a confiável providência in the CALLER's tx and
// returns its id — born OPEN/RULE, titled after the providência's tipo, inheriting the context
// FKs the actionitem row already carries (so no cross-table read is needed: the caller passes
// court_record_id directly, unlike the old async path that had to re-read it). Every providência
// gets a task, gera_peca or not (docs §2: "há o quê fazer: dar-se por ciente").
//
// Idempotent via migration 0087's UNIQUE (action_item_id): a second call for the same
// action_item (a re-confirm, a re-materialization that got past dedup) hits ON CONFLICT DO
// NOTHING → ErrTaskExistsForActionItem, and this reads back the existing task id instead of
// minting a second. No event is emitted here — the link is written synchronously by the caller
// (actionitem.LinkTask), not by a downstream listener.
func (c *ActionItemTaskCreator) CreateForActionItem(ctx context.Context, tx database.Tx, in actionitem.ActionItemTask) (string, error) {
	saved, err := c.repo.InsertTask(ctx, tx, &Task{
		TenantID:      in.TenantID,
		CourtRecordID: in.CourtRecordID,
		DeadlineID:    in.DeadlineID,
		IntimationID:  in.IntimationID,
		Title:         in.Tipo,
		Status:        TaskStatusOpen,
		Source:        SourceRule,
		ActionItemID:  in.ActionItemID,
	})
	if errors.Is(err, ErrTaskExistsForActionItem) {
		// The task already exists for this providência (0087's UNIQUE) — return its id so the
		// caller links the SAME task, never a duplicate.
		return c.repo.GetTaskIDByActionItem(ctx, tx, in.TenantID, in.ActionItemID)
	}
	if err != nil {
		return "", err
	}
	return saved.ID, nil
}

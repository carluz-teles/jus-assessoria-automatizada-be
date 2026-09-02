package actionitem

import (
	"context"

	"github.com/jusassessoria/platform/lib/database"
)

// TaskCreator is the SYNCHRONOUS port this slice calls to mint (or find) the tarefa of a
// confiável providência INSIDE the caller's own tx — no more async hop through the outbox
// (docs/erd-costura-providencia-tarefa-peca.md, revisão síncrona). internal/deadline
// implements it (ActionItemTaskCreator) and the api/worker composition injects the impl via
// WithTaskCreator. Defined HERE (not in deadline) so this slice never imports deadline — the
// dependency points inward, deadline→actionitem, never the reverse (deadline already imports
// this slice's event consts; a reverse import would cycle).
//
// CreateForActionItem is idempotent: called twice for the same action_item (a re-confirm, a
// re-materialization) it returns the SAME task id, never a duplicate — the impl leans on
// task.action_item_id's UNIQUE (migration 0087).
type TaskCreator interface {
	CreateForActionItem(ctx context.Context, tx database.Tx, in ActionItemTask) (taskID string, err error)
}

// ActionItemTask is the input the confiável providência hands the TaskCreator: the context
// ids the task inherits and the tipo that titles it. Every field is a plain string; an empty
// context id (no prazo/intimação bound yet) is a valid avulsa-ish task, the impl lifts "" to
// a NULL FK.
type ActionItemTask struct {
	TenantID      string
	ActionItemID  string
	CourtRecordID string
	DeadlineID    string
	IntimationID  string
	Tipo          string
}

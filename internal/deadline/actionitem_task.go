package deadline

import (
	"context"
	"errors"

	"github.com/jusassessoria/platform/lib/database"
)

// actionitem_task.go closes the providência → tarefa loop (docs/erd-costura-providencia-
// tarefa-peca.md §2/§6, the Architect's "Listener-driven" decision): when actionitem.created
// fires (a declarado/manual providência, born confiável) OR actionitem.confirmed fires (an
// ia-inferred one just confirmed), this slice creates the task AUTOMATICALLY — no HTTP call
// from the FE. Both events funnel through the SAME core (createTaskFromActionItem) because
// their downstream effect is identical; only the emitting moment differs (materialization vs.
// human confirmation). It REUSES the tasks.go write path's shape (InsertTask + task.created
// in the SAME tx) rather than forking a parallel task-creation path.

// consumerActionItemTask is the processed_event consumer this slice dedups
// actionitem.created/confirmed under. Dedup is per-consumer (docs §4c.3), kept distinct from
// consumerDeadline/consumerReconcile so this new consumption never collides with the
// intimation-derived flows on the same event id space.
const consumerActionItemTask = "deadline.action_item_task"

// OnActionItemCreated is actionitem.created's handler: the providência was born already
// confiável (declarado/manual), so its task is created right away. Delegates to the shared
// core; see createTaskFromActionItem for the steps.
func (uc *UseCase) OnActionItemCreated(ctx context.Context, ev ActionItemFact) error {
	return uc.createTaskFromActionItem(ctx, ev)
}

// OnActionItemConfirmed is actionitem.confirmed's handler: an ia-inferred providência just
// turned confiável by the lawyer's hand, so its task is created NOW (deferred from
// materialization time). Same downstream effect as OnActionItemCreated — delegates to the
// same shared core.
func (uc *UseCase) OnActionItemConfirmed(ctx context.Context, ev ActionItemFact) error {
	return uc.createTaskFromActionItem(ctx, ev)
}

// createTaskFromActionItem is the shared core behind both actionitem.created and
// actionitem.confirmed (docs §6's fluxo): in ONE tenant-scoped tx it dedups, reads the
// providência's court_record_id (decisão P1 — the event payload does not carry it),
// persists a task titled after the providência's tipo, born OPEN/RULE, and emits
// task.created carrying the action_item_id — so internal/actionitem's own listener can
// write the reverse pointer on ITS table.
//
// Every providência gets a task, gera_peca or not (docs §2: "há o quê fazer: dar-se por
// ciente" — a ciência-only item still needs a task, just with draft_id left NULL, which is a
// LATER slice's concern; this slice never sets draft_id). Idempotent via 0087's UNIQUE
// (action_item_id): a redelivered event that got past the dedup mark (e.g. a crash between
// commit and ack) still cannot mint a second task — InsertTask's ON CONFLICT DO NOTHING
// yields ErrTaskExistsForActionItem, treated here as a safe no-op.
func (uc *UseCase) createTaskFromActionItem(ctx context.Context, ev ActionItemFact) error {
	return uc.uow.Do(ctx, ev.TenantID, func(tx database.Tx) error {
		seen, err := uc.dedup.SeenOrMark(ctx, tx, consumerActionItemTask, ev.EventID)
		if err != nil {
			return err
		}
		if seen {
			return nil
		}

		courtRecordID, err := uc.repo.GetActionItemCourtRecordID(ctx, tx, ev.TenantID, ev.ActionItemID)
		if err != nil {
			return err
		}

		saved, err := uc.repo.InsertTask(ctx, tx, &Task{
			TenantID:      ev.TenantID,
			CourtRecordID: courtRecordID,
			DeadlineID:    derefString(ev.DeadlineID),
			IntimationID:  ev.IntimationID,
			Title:         ev.Tipo,
			Status:        TaskStatusOpen,
			Source:        SourceRule,
			ActionItemID:  ev.ActionItemID,
		})
		if errors.Is(err, ErrTaskExistsForActionItem) {
			// The dedup mark above still commits, so a genuine redelivery never reaches here
			// again; this guards the rarer crash-after-commit-before-ack window.
			return nil
		}
		if err != nil {
			return err
		}

		return uc.outbox.Publish(ctx, tx, newTaskCreatedFromTask(saved))
	})
}

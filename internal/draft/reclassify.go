package draft

import (
	"context"

	"github.com/hibiken/asynq"

	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// reclassify.go closes the reclassificação loop from the OTHER side (docs/erd-costura-
// providencia-tarefa-peca.md §7 questão 4, fatia 5's "descartar e recomeçar" decision):
// when internal/actionitem's Reclassificar use case flips a providência's tipo/
// piece_profile_key AFTER a peça may already exist for its task, THIS listener supersedes
// the OLD draft (superseded_at = now()) so a fresh POST /v1/pecas {task_id} call can mint
// the corrected one — Create's populateFromTask path (domain.go) then backfills the OLD
// draft's superseded_by_draft_id, closing the pointer chain old→new.
//
// internal/draft does NOT import internal/actionitem in production code (unlike
// internal/deadline, which safely imports actionitem's consts because actionitem never
// imports deadline back): internal/actionitem's OWN production code already imports
// internal/acquisition, and internal/acquisition's test suite imports internal/draft (its
// activity listener tests reference draft's produced-event consts) — so draft→actionitem
// would close a cycle at the acquisition test-binary level (acquisition.test →
// draft → actionitem → acquisition). TypeActionItemReclassified is therefore a STRING
// LITERAL, exactly mirroring how internal/actionitem/events.go's own TypeTaskCreated
// avoids importing internal/deadline for the identical reason. The literal is guarded
// against drift by a round-trip contract test (reclassify_test.go) that DOES import
// internal/actionitem — safe from a _test.go file, since nothing in the production build
// graph depends on draft's tests, so no cycle exists there.
//
// Kept as its own small use case (mirrors GenerateUseCase's separation in generate.go)
// rather than a method on the bigger UseCase: it has a narrow, self-contained dependency
// set and — unlike generation ("ai" queue) — mounts on the DEDICATED "deadline" queue
// (lib/events' queueFor routes actionitem.reclassified there, alongside
// actionitem.created/confirmed, for the same starvation-avoidance reason), so it never
// shares a mux with GenerateUseCase's Listener.

// TypeActionItemReclassified is the dotted id this slice CONSUMES from actionitem.
const TypeActionItemReclassified = "actionitem.reclassified"

// ActionItemReclassified is the LOCAL decode shape of actionitem.reclassified: the exact
// subset this slice's listener reads. actionitem's payload does NOT carry task_id (it is
// the SAME frozen shape actionitem.created/confirmed already use — only action_item_id),
// so task_id is resolved via a dedicated cross-slice read (GetTaskIDForActionItem) instead
// of widening that shared contract. This struct is only ever DECODED here (events.Decode
// needs no interface), so it carries no Type()/AggregateType(). Base yields the event id
// for dedup.
type ActionItemReclassified struct {
	events.Base
	ActionItemID string `json:"action_item_id"`
	TenantID     string `json:"tenant_id"`
}

// reclassifyDeduper is the narrow dedup port this use case needs — same shape as
// generateDeduper/txDeduper elsewhere in this slice (generate.go, listener.go).
type reclassifyDeduper interface {
	SeenOrMark(ctx context.Context, tx database.Tx, consumer, eventID string) (seen bool, err error)
}

// txReclassifyDeduper adapts lib/events.Dedup to the reclassifyDeduper port.
type txReclassifyDeduper struct{}

// NewReclassifyDeduper returns the reclassifyDeduper the use case uses.
func NewReclassifyDeduper() reclassifyDeduper { return txReclassifyDeduper{} }

func (txReclassifyDeduper) SeenOrMark(ctx context.Context, tx database.Tx, consumer, eventID string) (bool, error) {
	return events.NewDedup(tx).SeenOrMark(ctx, consumer, eventID)
}

// reclassifyConsumer is the processed_event consumer name this slice dedups
// actionitem.reclassified under (docs §4c.3 — per-consumer).
const reclassifyConsumer = "draft_reclassify"

// reclassifyRepo is the narrow read+write port ReclassifyUseCase needs — a subset of the
// full Repository (which also satisfies it structurally).
type reclassifyRepo interface {
	GetTaskIDForActionItem(ctx context.Context, tx database.Tx, tenantID, actionItemID string) (string, error)
	SupersedeDraftForTask(ctx context.Context, tx database.Tx, tenantID, taskID string) error
}

// ReclassifyUseCase is the async handler for actionitem.reclassified.
type ReclassifyUseCase struct {
	uow   database.UnitOfWork
	repo  reclassifyRepo
	dedup reclassifyDeduper
}

// NewReclassifyUseCase wires the use case.
func NewReclassifyUseCase(uow database.UnitOfWork, repo reclassifyRepo, dedup reclassifyDeduper) *ReclassifyUseCase {
	return &ReclassifyUseCase{uow: uow, repo: repo, dedup: dedup}
}

// OnActionItemReclassified is actionitem.reclassified's handler: in ONE tenant-scoped tx
// it dedups, resolves the providência's task, and supersedes that task's vigente draft (if
// any). No-op (success) when the providência has no task yet, or its draft is already
// superseded/filed (both are either a harmless race or simply "nothing to do" — the
// providência was reclassified before ever producing a peça).
func (uc *ReclassifyUseCase) OnActionItemReclassified(ctx context.Context, ev ActionItemReclassified) error {
	return uc.uow.Do(ctx, ev.TenantID, func(tx database.Tx) error {
		seen, err := uc.dedup.SeenOrMark(ctx, tx, reclassifyConsumer, ev.EventID)
		if err != nil {
			return err
		}
		if seen {
			return nil
		}

		taskID, err := uc.repo.GetTaskIDForActionItem(ctx, tx, ev.TenantID, ev.ActionItemID)
		if err != nil {
			return err
		}
		if taskID == "" {
			return nil
		}

		return uc.repo.SupersedeDraftForTask(ctx, tx, ev.TenantID, taskID)
	})
}

// reclassifyUseCase is the port ReclassifyListener delegates to.
type reclassifyUseCase interface {
	OnActionItemReclassified(ctx context.Context, ev ActionItemReclassified) error
}

// ReclassifyListener is the draft slice's asynq consumer for actionitem.reclassified. Kept
// separate from Listener (listener.go, generation pipeline) because the two mount on
// DIFFERENT dedicated queues/servers ("ai" vs "deadline") and are never composed together.
type ReclassifyListener struct {
	uc reclassifyUseCase
}

// NewReclassifyListener wires the listener to the reclassify use case.
func NewReclassifyListener(uc reclassifyUseCase) *ReclassifyListener {
	return &ReclassifyListener{uc: uc}
}

// Register mounts the actionitem.reclassified handler on the asynq mux. Called by the
// worker's composition — the DEDICATED "deadline" server's mux (lib/events' queueFor
// routes actionitem.reclassified there), alongside deadline.NewListener's own
// actionitem.created/confirmed handlers.
func (l *ReclassifyListener) Register(mux *asynq.ServeMux) {
	mux.HandleFunc(TypeActionItemReclassified, l.handleActionItemReclassified)
}

// handleActionItemReclassified decodes the payload and delegates to the use case. A
// decode error is already SkipRetry (events.Decode wraps it); any use-case error stays
// retryable (infra failures are the only expected error here — the use case's own guards
// return nil for every "nothing to do" case).
func (l *ReclassifyListener) handleActionItemReclassified(ctx context.Context, t *asynq.Task) error {
	ev, err := events.Decode[ActionItemReclassified](t)
	if err != nil {
		return err
	}
	return l.uc.OnActionItemReclassified(ctx, ev)
}

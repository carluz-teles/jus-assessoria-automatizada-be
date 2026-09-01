package actionitem

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// deduper is the consumer-side idempotency guard port (see dedup.go).
type deduper interface {
	SeenOrMark(ctx context.Context, tx database.Tx, consumer, eventID string) (seen bool, err error)
}

// publisher is the transactional-outbox port — *events.Outbox satisfies it structurally.
type publisher interface {
	Publish(ctx context.Context, tx database.Tx, ev events.Event) error
}

// UnitOfWork is the subset of database.UnitOfWork this slice needs; lib/database's real
// implementation satisfies it structurally.
type UnitOfWork interface {
	Do(ctx context.Context, tenantID string, fn func(tx database.Tx) error) error
}

// UseCase is the Providência slice's single use case: materialize candidates from
// acquisition's analysis event, and the confirmar/descartar HTTP actions. It depends only
// on the ports above and the UnitOfWork — never a concrete implementation.
type UseCase struct {
	repo   Repository
	outbox publisher
	dedup  deduper
	uow    UnitOfWork
	now    func() time.Time
}

// Option configures a UseCase at construction.
type Option func(*UseCase)

// WithClock overrides the reference clock used to stamp created_at/updated_at. Production
// leaves the default (time.Now); tests pin it for deterministic assertions.
func WithClock(now func() time.Time) Option {
	return func(uc *UseCase) { uc.now = now }
}

// NewUseCase wires the use case to its repository, outbox publisher, dedup guard and unit
// of work.
func NewUseCase(repo Repository, outbox publisher, dedup deduper, uow UnitOfWork, opts ...Option) *UseCase {
	uc := &UseCase{repo: repo, outbox: outbox, dedup: dedup, uow: uow, now: time.Now}
	for _, opt := range opts {
		opt(uc)
	}
	return uc
}

// OnIntimationAnalyzed materializes N action_item rows from ONE acquisition.intimation.
// analyzed event — one per providência candidate the IA identified — all in a single
// tenant-scoped transaction so the dedup mark and every insert commit together.
//
// Guard aditivo (docs handoff, "nunca destrutivo"): a re-analysis ("Gerar novamente")
// NEVER touches an item already bound to a task (task_id set) nor a confiável one already
// committed — DeleteReplaceableActionItems clears ONLY the SUGGESTED+a_confirmar+
// task_id-IS-NULL subset before the fresh candidates are inserted. A confiável
// (declarado/manual) candidate that already has a committed match — by (tipo, tipo_origem)
// — is skipped rather than duplicated (ExistsActionItemByTipo), since the delete above
// never clears those rows on a repeat run.
//
// Each candidate's tipo/gera_peca/piece_profile_key is sanitized (sanitizeCandidate) before
// it ever reaches the DB, so a hallucinated classifier output degrades ONE candidate to
// ciência/no-peça instead of failing the whole event on an FK violation (viés seguro,
// mirroring analise.go's resolveAssignee/clampDueDate). tipo_origem/tipo_status follow the
// motor de precedência (deriveTipoOrigemStatus): declarado items are born confiável and
// immediately emit actionitem.created (the future deadline listener creates their task
// right away); everything else is born a_confirmar and waits for Confirmar.
func (uc *UseCase) OnIntimationAnalyzed(ctx context.Context, ev IntimationAnalyzed) error {
	return uc.uow.Do(ctx, ev.TenantID, func(tx database.Tx) error {
		seen, err := uc.dedup.SeenOrMark(ctx, tx, consumerIntimationAnalyzed, ev.EventID)
		if err != nil {
			return err
		}
		if seen {
			return nil
		}

		if err := uc.repo.DeleteReplaceableActionItems(ctx, tx, ev.TenantID, ev.IntimationID); err != nil {
			return err
		}

		for _, c := range ev.Providencias {
			if err := uc.materializeCandidate(ctx, tx, ev, c); err != nil {
				return err
			}
		}
		return nil
	})
}

// materializeCandidate is the per-candidate body of OnIntimationAnalyzed: sanitize, dedup
// against a committed match, derive the precedence, insert, and (when born confiável)
// publish actionitem.created — all inside the caller's tx.
func (uc *UseCase) materializeCandidate(ctx context.Context, tx database.Tx, ev IntimationAnalyzed, c ProvidenciaCandidate) error {
	tipo, geraPeca, pieceProfileKey := sanitizeCandidate(c.Tipo, c.GeraPeca, derefString(c.PieceProfileKey))
	tipoOrigem, tipoStatus := deriveTipoOrigemStatus(c.Declarado)

	exists, err := uc.repo.ExistsActionItemByTipo(ctx, tx, ev.TenantID, ev.IntimationID, tipo, tipoOrigem)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}

	var confianca *float64
	if tipoOrigem == TipoOrigemIA {
		confianca = clampConfianca(c.Confianca)
	}

	now := uc.now()
	item := &ActionItem{
		ID:              uuid.Must(uuid.NewV7()).String(),
		TenantID:        ev.TenantID,
		IntimationID:    ev.IntimationID,
		CourtRecordID:   ev.CourtRecordID,
		Tipo:            tipo,
		GeraPeca:        geraPeca,
		PieceProfileKey: pieceProfileKey,
		TipoOrigem:      tipoOrigem,
		TipoStatus:      tipoStatus,
		DeadlineID:      ev.DeadlineID,
		Confianca:       confianca,
		Status:          StatusSuggested,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := item.validate(); err != nil {
		return err
	}

	saved, err := uc.repo.InsertActionItem(ctx, tx, item)
	if err != nil {
		return err
	}

	if tipoStatus != TipoStatusConfiavel {
		return nil
	}
	return uc.outbox.Publish(ctx, tx, newActionItemCreated(saved))
}

// Confirmar is POST /v1/action-items/:id/confirmar: tipo_status a_confirmar → confiável,
// emitting actionitem.confirmed in the same tx. Idempotent: an already-confiável item is a
// no-op success (the item is returned unchanged, no event re-emitted). Confirming an
// already-DISCARDED item is a CONFLICT — descartar is terminal, so confirmar can never
// resurrect it.
func (uc *UseCase) Confirmar(ctx context.Context, tenantID, id string) (*ActionItem, error) {
	var result *ActionItem
	err := uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		item, err := uc.repo.GetActionItem(ctx, tx, tenantID, id)
		if err != nil {
			return err
		}
		if item.Status == StatusDiscarded {
			return ErrActionItemDiscarded
		}
		if item.TipoStatus == TipoStatusConfiavel {
			result = item
			return nil
		}

		confirmed, err := uc.repo.ConfirmActionItem(ctx, tx, tenantID, id)
		if err != nil {
			return err
		}
		if err := uc.outbox.Publish(ctx, tx, newActionItemConfirmed(confirmed)); err != nil {
			return err
		}
		result = confirmed
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// Descartar is POST /v1/action-items/:id/descartar: status → DISCARDED, emitting
// actionitem.discarded in the same tx. Idempotent: redescartar an already-DISCARDED item
// is a no-op success.
func (uc *UseCase) Descartar(ctx context.Context, tenantID, id string) (*ActionItem, error) {
	var result *ActionItem
	err := uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		item, err := uc.repo.GetActionItem(ctx, tx, tenantID, id)
		if err != nil {
			return err
		}
		if item.Status == StatusDiscarded {
			result = item
			return nil
		}

		discarded, err := uc.repo.DiscardActionItem(ctx, tx, tenantID, id)
		if err != nil {
			return err
		}
		if err := uc.outbox.Publish(ctx, tx, newActionItemDiscarded(discarded)); err != nil {
			return err
		}
		result = discarded
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// Reclassificar is POST /v1/action-items/:id/reclassificar (fatia 5, docs §7 questão 4):
// the advogado overrides the providência's tipo/piece_profile_key AFTER a peça may already
// have been generated from it. The Architect's decision is "descartar e recomeçar": this
// use case only flips the providência's own row and announces actionitem.reclassified —
// internal/draft's listener reacts by superseding the OLD draft, and the NEXT
// POST /v1/pecas {task_id} mints the corrected one. This use case never touches draft/task
// tables directly (slices only talk by event/SQL-read, never by importing each other's
// domain).
//
// Guards, in order:
//  1. piece_profile_key/tipo must be members of the known closed sets — checked here in Go
//     BEFORE the UPDATE reaches the FK/CHECK, so a bad value is a client 400
//     (ErrUnknownPieceProfileKey/ErrInvalidTipoReclassify), never an opaque FK-violation 500.
//     The handler's ozzo Validate() already catches this at the edge; this is the domain-
//     layer belt (defends a direct non-HTTP caller too).
//  2. status != DISCARDED — a dismissed providência can never be reclassified (mirrors
//     Confirmar's guard).
//  3. HasFiledDraftForActionItem — once the peça has been FILED (protocolada), the
//     providência is frozen: reclassifying now would orphan a filed document with no paper
//     trail of why the tipo changed. ErrActionItemHasFiledDraft (409); no UPDATE happens.
//
// Idempotent: if (piece_profile_key, tipo) already match AND tipo_origem is already
// "manual", the row is returned unchanged and actionitem.reclassified is NOT re-emitted
// (mirrors Confirmar's idempotent-no-op).
func (uc *UseCase) Reclassificar(ctx context.Context, tenantID, id, pieceProfileKey, tipo string) (*ActionItem, error) {
	if !validPieceProfileKey(pieceProfileKey) {
		return nil, ErrUnknownPieceProfileKey
	}
	if !validTipo(tipo) {
		return nil, ErrInvalidTipoReclassify
	}

	var result *ActionItem
	err := uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		item, err := uc.repo.GetActionItem(ctx, tx, tenantID, id)
		if err != nil {
			return err
		}
		if item.Status == StatusDiscarded {
			return ErrActionItemDiscarded
		}

		hasFiled, err := uc.repo.HasFiledDraftForActionItem(ctx, tx, tenantID, id)
		if err != nil {
			return err
		}
		if hasFiled {
			return ErrActionItemHasFiledDraft
		}

		if item.PieceProfileKey == pieceProfileKey && item.Tipo == tipo && item.TipoOrigem == TipoOrigemManual {
			result = item
			return nil
		}

		reclassified, err := uc.repo.ReclassifyActionItem(ctx, tx, tenantID, id, pieceProfileKey, tipo)
		if err != nil {
			return err
		}
		if err := uc.outbox.Publish(ctx, tx, newActionItemReclassified(reclassified)); err != nil {
			return err
		}
		result = reclassified
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// consumerTaskCreated is the processed_event consumer this slice dedups deadline's
// task.created under (docs §4c.3) — distinct from consumerIntimationAnalyzed so marking here
// never blocks that other consumption of a DIFFERENT event.
const consumerTaskCreated = "actionitem.task_created"

// OnTaskCreated is deadline.task.created's handler — the reverse half of the providência→
// tarefa loop (docs §2/§6, fatia 3): when the task carries an action_item_id, this writes
// task_id + status=CONFIRMED onto THIS slice's own action_item row (never deadline's task
// row — each slice only ever writes its own table). A task.created with no action_item_id
// (the vast majority: manual/avulsa tasks) is skipped before any tx opens — not this slice's
// concern, and never worth a dedup mark.
func (uc *UseCase) OnTaskCreated(ctx context.Context, ev TaskCreated) error {
	if ev.ActionItemID == "" {
		return nil
	}
	return uc.uow.Do(ctx, ev.TenantID, func(tx database.Tx) error {
		seen, err := uc.dedup.SeenOrMark(ctx, tx, consumerTaskCreated, ev.EventID)
		if err != nil {
			return err
		}
		if seen {
			return nil
		}

		_, err = uc.repo.LinkTask(ctx, tx, ev.TenantID, ev.ActionItemID, ev.TaskID)
		if errors.Is(err, ErrActionItemNotFound) {
			// Redelivered past the dedup mark (e.g. a crash between commit and ack), or a
			// genuinely gone action_item — either way task_id is already linked/irrelevant, a
			// safe no-op (mirrors deadline's own OnIntimationCancelled treatment of its
			// not-found).
			return nil
		}
		return err
	})
}

// clampConfianca keeps a classifier-reported confidence within [0, 1] — a nil input passes
// through (no confidence reported), an out-of-range value is clamped rather than rejected
// (viés seguro: a malformed score should not fail the whole materialization).
func clampConfianca(c *float64) *float64 {
	if c == nil {
		return nil
	}
	v := *c
	switch {
	case v < 0:
		v = 0
	case v > 1:
		v = 1
	}
	return &v
}

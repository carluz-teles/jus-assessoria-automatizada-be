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
	// taskCreator is the SYNCHRONOUS port that mints (or finds) the tarefa of a confiável
	// providência inside the SAME tx — injected via WithTaskCreator by the api/worker
	// composition (internal/deadline's ActionItemTaskCreator). When nil, the slice falls back
	// to the legacy async path (emit actionitem.created/confirmed and let a listener create the
	// task later); both production call sites inject it, so the fallback only survives for tests
	// that don't wire it.
	taskCreator TaskCreator
}

// Option configures a UseCase at construction.
type Option func(*UseCase)

// WithClock overrides the reference clock used to stamp created_at/updated_at. Production
// leaves the default (time.Now); tests pin it for deterministic assertions.
func WithClock(now func() time.Time) Option {
	return func(uc *UseCase) { uc.now = now }
}

// WithTaskCreator injects the synchronous task-creation port (internal/deadline's adapter in
// production). Both call sites (cmd/api confirmar, cmd/worker-ingestao materialization) pass
// it; a UseCase built without it degrades to the legacy async event emission.
func WithTaskCreator(tc TaskCreator) Option {
	return func(uc *UseCase) { uc.taskCreator = tc }
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

	exists, err := uc.repo.ExistsActionItemByTipo(ctx, tx, ev.TenantID, ev.IntimationID, tipo)
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
		Title:           c.Title,
		Description:     c.Description,
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

	// Only a confiável (declarado/manual) candidate gets its task NOW. An IA candidate
	// (a_confirmar) waits for Confirmar. A ciência-only item still gets a task (docs §2:
	// "há o quê fazer: dar-se por ciente").
	if tipoStatus != TipoStatusConfiavel {
		return nil
	}
	_, err = uc.createAndLinkTask(ctx, tx, saved)
	return err
}

// createAndLinkTask is the SYNCHRONOUS providência→tarefa core shared by materializeCandidate
// (declarado, born confiável) and Confirmar (IA item just confirmed): inside the caller's tx it
// asks the injected TaskCreator to mint (or find, idempotently) the task, then writes the
// reverse pointer onto THIS slice's row (task_id + status=CONFIRMED via LinkTask). Both the task
// and the pointer commit together with the action_item change — no async hop, no fila that could
// starve a user-driven action. Returns the linked item.
//
// Falls back to the legacy async path (emit the event) ONLY when no TaskCreator was injected —
// production always injects one; this keeps unit tests that don't wire it source-compatible.
func (uc *UseCase) createAndLinkTask(ctx context.Context, tx database.Tx, item *ActionItem) (*ActionItem, error) {
	if uc.taskCreator == nil {
		return item, uc.outbox.Publish(ctx, tx, newActionItemCreated(item))
	}

	taskID, err := uc.taskCreator.CreateForActionItem(ctx, tx, ActionItemTask{
		TenantID:      item.TenantID,
		ActionItemID:  item.ID,
		CourtRecordID: item.CourtRecordID,
		DeadlineID:    item.DeadlineID,
		IntimationID:  item.IntimationID,
		Tipo:          item.Tipo,
	})
	if err != nil {
		return nil, err
	}

	linked, err := uc.repo.LinkTask(ctx, tx, item.TenantID, item.ID, taskID)
	if errors.Is(err, ErrActionItemNotFound) {
		// The row is already linked/gone — a safe no-op (mirrors the old async listener's
		// treatment of a redelivered task.created). Return the item as it stands.
		return item, nil
	}
	if err != nil {
		return nil, err
	}
	return linked, nil
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
		// Already confiável WITH a task linked → nothing left to do (idempotent no-op).
		if item.TipoStatus == TipoStatusConfiavel && item.TaskID != "" {
			result = item
			return nil
		}

		// Flip a_confirmar→confiável (skipped if it is already confiável but somehow unlinked —
		// then we only need to create+link its task now).
		confirmed := item
		if item.TipoStatus != TipoStatusConfiavel {
			confirmed, err = uc.repo.ConfirmActionItem(ctx, tx, tenantID, id)
			if err != nil {
				return err
			}
		}

		// Create the task and write the reverse pointer SYNCHRONOUSLY in this same tx — no
		// actionitem.confirmed event, no async hop (the user is waiting on this action).
		linked, err := uc.createAndLinkTask(ctx, tx, confirmed)
		if err != nil {
			return err
		}
		result = linked
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

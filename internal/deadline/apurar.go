package deadline

import (
	"context"
	"fmt"
	"time"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/pkg/tribunal"
)

// apurar.go is the human decision path over an a_apurar prazo (V1, docs/design-motor-de-prazos-
// v1.md §"Divergência"/§"Fallback IA"): POST /v1/prazos/:id/apurar-divergencia resolves a
// declarado×calculado divergência, POST /v1/prazos/:id/apurar-tipo confirms or reclassifies the
// IA-inferred tipo de ato. Both flip selo a_apurar → confiavel and emit deadline.seal_assigned
// (REUSED from birth, events.go newDeadlineSealAssigned — no new event type). Origem is NEVER
// touched here — the Architect's decision is that origem is immutable after creation (only
// domain.go's OnIntimationObserved writes it); apurar.go only ever reads it back to stamp the
// unchanged value onto the event. It mirrors adjust.go's shape (a UseCase method per command,
// the SAME uow/outbox transactional-outbox pattern) and, for the ajuste_manual decisão, REUSES
// adjust.go's private resolveStart/computeWithExtra helpers directly (not the public Adjust())
// because the apuração must write cross_validation in the SAME tx.

// apurarDecisao is the closed set POST /v1/prazos/:id/apurar-divergencia accepts.
type apurarDecisao string

const (
	decisaoAceitaDeclarado apurarDecisao = "aceita_declarado"
	decisaoAceitaCalculado apurarDecisao = "aceita_calculado"
	decisaoAjusteManual    apurarDecisao = "ajuste_manual"
)

// validApurarDecisao reports whether d is a member of the closed apurarDecisao set.
func validApurarDecisao(d apurarDecisao) bool {
	switch d {
	case decisaoAceitaDeclarado, decisaoAceitaCalculado, decisaoAjusteManual:
		return true
	}
	return false
}

// apurarTipoAcao is the closed set POST /v1/prazos/:id/apurar-tipo accepts.
type apurarTipoAcao string

const (
	acaoConfirmar     apurarTipoAcao = "confirmar"
	acaoReclassificar apurarTipoAcao = "reclassificar"
)

// validApurarTipoAcao reports whether a is a member of the closed apurarTipoAcao set.
func validApurarTipoAcao(a apurarTipoAcao) bool {
	switch a {
	case acaoConfirmar, acaoReclassificar:
		return true
	}
	return false
}

// decisaoLabel is the human "trilha" phrase for an apurarDecisao — never the raw enum value
// (mirrors domain.go's anchorEventLabel/countingLabel pattern).
func decisaoLabel(d apurarDecisao) string {
	switch d {
	case decisaoAceitaDeclarado:
		return "declarado aceito"
	case decisaoAceitaCalculado:
		return "calculado aceito"
	case decisaoAjusteManual:
		return "ajuste manual aplicado"
	default:
		return string(d)
	}
}

// acaoLabel is the human "trilha" phrase for an apurarTipoAcao — never the raw enum value.
func acaoLabel(a apurarTipoAcao) string {
	switch a {
	case acaoConfirmar:
		return "confirmado"
	case acaoReclassificar:
		return "reclassificado"
	default:
		return string(a)
	}
}

// ApurarDivergenciaCommand is the apurar-divergencia input the handler builds from the request +
// the verified principal (TenantID/UserID NEVER from the body). The AjusteManual* fields are
// used ONLY when Decisao=="ajuste_manual", mirroring AdjustCommand's pointer-merge idiom (nil
// keeps the prazo's stored value — a partial patch over the CURRENT {days, counting, doubled,
// anchor_event, manual_extra_days}).
type ApurarDivergenciaCommand struct {
	TenantID        string
	UserID          string
	DeadlineID      string
	Decisao         apurarDecisao
	Days            *int
	Counting        *Counting
	Doubled         *bool
	AnchorEvent     *AnchorEvent
	ManualExtraDays *int
}

// ApuradoDivergencia is the apurar-divergencia outcome: the prazo's (possibly recomputed)
// end_date, the flipped selo and the decisão recorded.
type ApuradoDivergencia struct {
	ID      string
	EndDate time.Time
	Seal    Seal
	Decisao apurarDecisao
}

// ApurarTipoCommand is the apurar-tipo input the handler builds from the request + the verified
// principal. Tipo is used ONLY when Acao=="reclassificar" (required then); "confirmar" reads the
// stored calc_memory.ia_tipo_inferido and stamps it as human-confirmed.
type ApurarTipoCommand struct {
	TenantID   string
	UserID     string
	DeadlineID string
	Acao       apurarTipoAcao
	Tipo       *string
}

// ApuradoTipo is the apurar-tipo outcome: the confirmed/reclassified tipo and the flipped selo.
type ApuradoTipo struct {
	ID   string
	Tipo string
	Seal Seal
}

// ApurarDivergencia resolves a declarado×calculado divergência (§"Divergência"): the human
// picks aceita_declarado (end_date := cross_validation.data_declarada, no recompute),
// aceita_calculado (end_date left as-is — it already IS the calculado date), or ajuste_manual
// (a fresh recompute via the SAME resolveStart/computeWithExtra idiom Adjust uses). All three
// branches then record the decisão + flip selo → confiavel + append a deadline_event + emit
// deadline.seal_assigned, in ONE tenant-scoped tx.
//
// Guards (idempotent, never a silent no-op):
//  1. the prazo must exist and be non-terminal (not MET/CANCELLED) — ErrDeadlineNotApuravel;
//  2. a cross_validation row must exist, be "divergente" AND still undecided (Decisao=="") —
//     ErrDeadlineNotDivergent otherwise (also the SECOND-call idempotency guard: a resolved
//     divergência refuses a re-apuração instead of reprocessing it).
func (uc *UseCase) ApurarDivergencia(ctx context.Context, cmd ApurarDivergenciaCommand) (ApuradoDivergencia, error) {
	if !validApurarDecisao(cmd.Decisao) {
		return ApuradoDivergencia{}, apperr.NewInvalid("decisao must be aceita_declarado, aceita_calculado or ajuste_manual")
	}

	var result ApuradoDivergencia
	err := uc.uow.Do(ctx, cmd.TenantID, func(tx database.Tx) error {
		cur, err := uc.repo.GetDeadlineForAdjust(ctx, tx, cmd.DeadlineID, cmd.TenantID)
		if err != nil {
			return err
		}
		if cur.Status == StatusMet || cur.Status == StatusCancelled {
			return ErrDeadlineNotApuravel
		}

		cv, err := uc.repo.GetCrossValidation(ctx, tx, cmd.TenantID, cmd.DeadlineID)
		if err != nil {
			return err
		}
		if cv.Resultado != crossValidationDivergente || cv.Decisao != "" {
			return ErrDeadlineNotDivergent
		}

		newEndDate := cv.DataCalculada
		switch cmd.Decisao {
		case decisaoAceitaDeclarado:
			newEndDate = cv.DataDeclarada
			// The internal buffer must stay in sync with the human-picked end_date, via the
			// SAME uf/court every other recompute in this slice uses — never crude arithmetic.
			court, err := uc.repo.GetCourtRecordCourt(ctx, tx, cmd.TenantID, cur.CourtRecordID)
			if err != nil {
				return err
			}
			uf := tribunal.UF(court)
			prazoInterno, _, err := uc.cal.SubtractBusinessDays(ctx, newEndDate, internalBufferBusinessDays, uf, court)
			if err != nil {
				return err
			}
			if err := uc.repo.UpdateDeadlineEndDate(ctx, tx, cmd.TenantID, cmd.DeadlineID, newEndDate, prazoInterno); err != nil {
				return err
			}
		case decisaoAceitaCalculado:
			// newEndDate already defaults to cv.DataCalculada; nothing to write — the stored
			// end_date already IS the calculado date.
		case decisaoAjusteManual:
			endDate, err := uc.apurarAjusteManual(ctx, tx, cmd, cur)
			if err != nil {
				return err
			}
			newEndDate = endDate
		}

		if err := uc.repo.UpdateCrossValidationDecision(ctx, tx, cmd.TenantID, cmd.DeadlineID, string(cmd.Decisao), cmd.UserID); err != nil {
			return err
		}
		if err := uc.repo.UpdateDeadlineSelo(ctx, tx, cmd.TenantID, cmd.DeadlineID, SealConfiavel, cmd.UserID, uc.now()); err != nil {
			return err
		}

		de := &DeadlineEvent{
			TenantID:   cmd.TenantID,
			DeadlineID: cmd.DeadlineID,
			Tipo:       "validado",
			Detalhe:    fmt.Sprintf("Divergência apurada: %s", decisaoLabel(cmd.Decisao)),
			AtorID:     cmd.UserID,
			Em:         uc.now(),
		}
		if err := uc.repo.InsertDeadlineEvent(ctx, tx, de); err != nil {
			return err
		}

		sealed := &Deadline{ID: cmd.DeadlineID, Origem: cur.Origem, Seal: SealConfiavel}
		if err := uc.outbox.Publish(ctx, tx, newDeadlineSealAssigned(sealed)); err != nil {
			return err
		}

		result = ApuradoDivergencia{ID: cmd.DeadlineID, EndDate: newEndDate, Seal: SealConfiavel, Decisao: cmd.Decisao}
		return nil
	})
	if err != nil {
		return ApuradoDivergencia{}, err
	}
	return result, nil
}

// apurarAjusteManual is the ajuste_manual branch of ApurarDivergencia: it merges the command's
// partial patch over cur (nil keeps the stored value, mirroring Adjust's merge), then REUSES
// resolveStart + computeWithExtra (adjust.go/domain.go's private recompute idiom) directly —
// NOT the public Adjust(), because ApurarDivergencia needs the SAME tx to also write
// cross_validation, which Adjust's signature does not carry.
func (uc *UseCase) apurarAjusteManual(ctx context.Context, tx database.Tx, cmd ApurarDivergenciaCommand, cur *DeadlineForAdjust) (time.Time, error) {
	days := cur.Days
	if cmd.Days != nil {
		days = *cmd.Days
	}
	counting := cur.Counting
	if cmd.Counting != nil {
		counting = *cmd.Counting
	}
	doubled := cur.Doubled
	if cmd.Doubled != nil {
		doubled = *cmd.Doubled
	}
	anchorEvent := cur.AnchorEvent
	if cmd.AnchorEvent != nil {
		anchorEvent = *cmd.AnchorEvent
	}
	manualExtraDays := cur.ManualExtraDays
	if cmd.ManualExtraDays != nil {
		manualExtraDays = *cmd.ManualExtraDays
	}

	court, err := uc.repo.GetCourtRecordCourt(ctx, tx, cmd.TenantID, cur.CourtRecordID)
	if err != nil {
		return time.Time{}, err
	}
	uf := tribunal.UF(court)

	start, err := uc.resolveStart(ctx, tx, cur.IntimationID, cmd.TenantID, anchorEvent, cur.StartDate)
	if err != nil {
		return time.Time{}, err
	}

	endDate, holidays, err := uc.computeWithExtra(ctx, counting, start, days, doubled, manualExtraDays, uf, court)
	if err != nil {
		return time.Time{}, err
	}
	if !endDate.After(start) {
		return time.Time{}, apperr.NewInvalid("deadline end date must be after start date")
	}

	// V1: recompute the internal safety buffer in lockstep with EndDate, via the SAME uf/
	// court already resolved above — never left stale against the recomputed EndDate.
	prazoInterno, _, err := uc.cal.SubtractBusinessDays(ctx, endDate, internalBufferBusinessDays, uf, court)
	if err != nil {
		return time.Time{}, err
	}

	if _, _, err := uc.repo.UpdateDeadlineAdjust(ctx, tx, UpdateDeadlineAdjustParams{
		DeadlineID:      cmd.DeadlineID,
		TenantID:        cmd.TenantID,
		Kind:            cur.Kind,
		Days:            days,
		Counting:        counting,
		Doubled:         doubled,
		DoubledReason:   cur.DoubledReason,
		EndDate:         endDate,
		PrazoInterno:    prazoInterno,
		HolidaysApplied: holidays,
		StartDate:       start,
		AnchorEvent:     anchorEvent,
		ManualExtraDays: manualExtraDays,
	}); err != nil {
		return time.Time{}, err
	}
	return endDate, nil
}

// ApurarTipo confirms or reclassifies the IA-inferred tipo de ato (§"Fallback IA"): "confirmar"
// stamps the stored calc_memory.ia_tipo_inferido as human-confirmed (ia_confianca → 1.0);
// "reclassificar" overrides it with the human's Tipo. Both flip selo a_apurar → confiavel,
// append a deadline_event and emit deadline.seal_assigned, in ONE tenant-scoped tx.
//
// Guards: the prazo must exist and be non-terminal (ErrDeadlineNotApuravel), and its selo must
// still be a_apurar (ErrDeadlineNotDivergent otherwise — also the idempotency guard: a second
// apuração on an already-confiavel prazo is refused, not reprocessed).
func (uc *UseCase) ApurarTipo(ctx context.Context, cmd ApurarTipoCommand) (ApuradoTipo, error) {
	if !validApurarTipoAcao(cmd.Acao) {
		return ApuradoTipo{}, apperr.NewInvalid("acao must be confirmar or reclassificar")
	}
	if cmd.Acao == acaoReclassificar && (cmd.Tipo == nil || *cmd.Tipo == "") {
		return ApuradoTipo{}, apperr.NewInvalid("tipo is required to reclassificar")
	}

	var result ApuradoTipo
	err := uc.uow.Do(ctx, cmd.TenantID, func(tx database.Tx) error {
		cur, err := uc.repo.GetDeadlineForAdjust(ctx, tx, cmd.DeadlineID, cmd.TenantID)
		if err != nil {
			return err
		}
		if cur.Status == StatusMet || cur.Status == StatusCancelled {
			return ErrDeadlineNotApuravel
		}
		if cur.Selo != SealAApurar {
			return ErrDeadlineNotDivergent
		}

		cm, err := uc.repo.GetCalcMemory(ctx, tx, cmd.TenantID, cmd.DeadlineID)
		if err != nil {
			return err
		}

		tipo := cm.IATipoInferido
		if cmd.Acao == acaoReclassificar {
			tipo = *cmd.Tipo
		}
		const confiancaHumanoConfirmado = 1.0
		if err := uc.repo.UpdateCalcMemoryTipoConfirmation(ctx, tx, cmd.TenantID, cmd.DeadlineID, tipo, confiancaHumanoConfirmado); err != nil {
			return err
		}
		if err := uc.repo.UpdateDeadlineSelo(ctx, tx, cmd.TenantID, cmd.DeadlineID, SealConfiavel, cmd.UserID, uc.now()); err != nil {
			return err
		}

		de := &DeadlineEvent{
			TenantID:   cmd.TenantID,
			DeadlineID: cmd.DeadlineID,
			Tipo:       "confirmado",
			Detalhe:    fmt.Sprintf("Tipo apurado: %s (%s)", tipo, acaoLabel(cmd.Acao)),
			AtorID:     cmd.UserID,
			Em:         uc.now(),
		}
		if err := uc.repo.InsertDeadlineEvent(ctx, tx, de); err != nil {
			return err
		}

		sealed := &Deadline{ID: cmd.DeadlineID, Origem: cur.Origem, Seal: SealConfiavel}
		if err := uc.outbox.Publish(ctx, tx, newDeadlineSealAssigned(sealed)); err != nil {
			return err
		}

		result = ApuradoTipo{ID: cmd.DeadlineID, Tipo: tipo, Seal: SealConfiavel}
		return nil
	})
	if err != nil {
		return ApuradoTipo{}, err
	}
	return result, nil
}

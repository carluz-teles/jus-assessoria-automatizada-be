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
// v1.md §"Divergência"): POST /v1/prazos/:id/apurar-divergencia resolves a declarado×calculado
// divergência, flipping selo a_apurar → confiavel and emitting deadline.seal_assigned (REUSED
// from birth, events.go newDeadlineSealAssigned — no new event type). Origem is NEVER touched
// here — the Architect's decision is that origem is immutable after creation (only domain.go's
// OnIntimationObserved writes it); apurar.go only ever reads it back to stamp the unchanged
// value onto the event. It mirrors adjust.go's shape (a UseCase method per command, the SAME
// uow/outbox transactional-outbox pattern); the ajuste_manual decisão sets a lawyer-picked data
// fatal (end_date) directly — the SAME shape as aceita_declarado (a chosen date), recomputing
// only the internal safety buffer in lockstep.

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

// ApurarDivergenciaCommand is the apurar-divergencia input the handler builds from the request +
// the verified principal (TenantID/UserID NEVER from the body). EndDate is used ONLY when
// Decisao=="ajuste_manual": the specific data fatal the lawyer picked, set directly (NOT
// recomputed from days/counting) — mirroring how aceita_declarado sets a chosen date.
type ApurarDivergenciaCommand struct {
	TenantID   string
	UserID     string
	DeadlineID string
	Decisao    apurarDecisao
	EndDate    *time.Time
}

// ApuradoDivergencia is the apurar-divergencia outcome: the prazo's (possibly recomputed)
// end_date, the flipped selo and the decisão recorded.
type ApuradoDivergencia struct {
	ID      string
	EndDate time.Time
	Seal    Seal
	Decisao apurarDecisao
}

// ApurarDivergencia resolves a declarado×calculado divergência (§"Divergência"): the human
// picks aceita_declarado (end_date := cross_validation.data_declarada, no recompute),
// aceita_calculado (end_date left as-is — it already IS the calculado date), or ajuste_manual
// (end_date := the lawyer-picked data fatal, set directly — no recompute). All three
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
			// The lawyer picks a specific data fatal directly — set it as-is (no recompute),
			// exactly like decisaoAceitaDeclarado sets cv.DataDeclarada.
			if cmd.EndDate == nil {
				return apperr.NewInvalid("end_date é obrigatório para ajuste manual")
			}
			newEndDate = *cmd.EndDate
			if !newEndDate.After(cur.StartDate) {
				return apperr.NewInvalid("a data do prazo deve ser posterior ao termo inicial")
			}
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

package deadline

import (
	"context"

	"github.com/jusassessoria/platform/lib/database"
)

// no_deadline.go is the "mera ciência" branch of the confirmation panel (§3 "Máquina de estados
// do prazo"): "Remover prazo" (from OPEN) and "Não há prazo" (from PENDING) both land a prazo in
// NO_DEADLINE — the human declaring the intimação carries no prazo to cumprir. It is DISTINCT
// from CANCELLED (revocation by a retificação event, event-driven), and REVERSIBLE via Reopen
// (NO_DEADLINE → PENDING). It reuses the same uow/outbox transactional-outbox idiom and the
// guarded-UPDATE concurrency floor as MarkMet/MarkMissed.

// NoDeadline flips a prazo PENDING|OPEN → NO_DEADLINE in ONE tenant-scoped tx, stamping
// confirmed_by/at and emitting deadline.no_deadline. It mirrors markStatus's shape: it pre-reads
// the status to distinguish a 404 miss (ErrDeadlineNotFound) from a 409 terminal
// (ErrDeadlineNotOpen — MET/MISSED/CANCELLED cannot become mera ciência), then the guarded
// UPDATE (status IN ('PENDING','OPEN')) is the concurrency floor. tenantID/userID come from the
// verified principal, never the body.
func (uc *UseCase) NoDeadline(ctx context.Context, tenantID, userID, deadlineID string) (MarkedDeadline, error) {
	var result MarkedDeadline
	err := uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		cur, err := uc.repo.GetDeadlineForCheck(ctx, tx, deadlineID, tenantID)
		if err != nil {
			return err
		}
		if cur.Status != StatusPending && cur.Status != StatusOpen {
			// MET / MISSED / CANCELLED — terminal; only CANCELLED (retificação) or a real
			// completion may leave those. Reuse ErrDeadlineNotOpen's 409 (the prazo exists but
			// its state forbids the flip), the same signal the met/missed guard uses.
			return ErrDeadlineNotOpen
		}

		id, err := uc.repo.MarkNoDeadline(ctx, tx, deadlineID, tenantID, userID, uc.now())
		if err != nil {
			return err
		}

		if err := uc.outbox.Publish(ctx, tx, newDeadlineNoDeadline(tenantID, id)); err != nil {
			return err
		}
		result = MarkedDeadline{ID: id, Status: StatusNoDeadline}
		return nil
	})
	if err != nil {
		return MarkedDeadline{}, err
	}
	return result, nil
}

// Reopen reverts a NO_DEADLINE prazo → PENDING in ONE tenant-scoped tx, clearing confirmed_by/at
// and emitting deadline.reopened — the undo of NoDeadline. It pre-reads the status to distinguish
// a 404 miss from a 409 not-NO_DEADLINE (ErrDeadlineNotReopenable); the guarded UPDATE
// (status = 'NO_DEADLINE') is the concurrency floor. tenantID comes from the verified principal.
func (uc *UseCase) Reopen(ctx context.Context, tenantID, deadlineID string) (MarkedDeadline, error) {
	var result MarkedDeadline
	err := uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		cur, err := uc.repo.GetDeadlineForCheck(ctx, tx, deadlineID, tenantID)
		if err != nil {
			return err
		}
		if cur.Status != StatusNoDeadline {
			return ErrDeadlineNotReopenable
		}

		id, err := uc.repo.ReopenNoDeadline(ctx, tx, deadlineID, tenantID)
		if err != nil {
			return err
		}

		if err := uc.outbox.Publish(ctx, tx, newDeadlineReopened(tenantID, id)); err != nil {
			return err
		}
		result = MarkedDeadline{ID: id, Status: StatusPending}
		return nil
	})
	if err != nil {
		return MarkedDeadline{}, err
	}
	return result, nil
}

package deadline

import (
	"context"
	"time"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
	"github.com/jusassessoria/platform/pkg/tribunal"
)

// adjust.go is the F2 ajuste + manual transitions of an ALREADY-derived prazo (docs/erd-prazos.md
// §9): PATCH /v1/prazos/:id recalculates the dates from a partial change to {days, counting,
// doubled, doubled_reason, kind}, and POST .../met | .../missed flip the lifecycle
// (OPEN→MET / OPEN→MISSED). It is the sibling of confirm.go: it REUSES the same recompute idiom
// (uc.compute → effectiveDays → lib/calendar, UF from pkg/tribunal), the same uow/outbox
// transactional-outbox pattern, and the same deadline.updated / deadline.missed events — it
// never duplicates the calendar math nor forks a parallel event contract.

// AdjustCommand is the ajuste input the handler builds from the request + the verified
// principal (docs §9, PATCH /v1/prazos/:id). TenantID and UserID come from the principal,
// NEVER the body (tenant isolation cannot be spoofed); DeadlineID keys the prazo. Every field
// is a POINTER so "present in the body" is distinguishable from "zero value": a nil field
// keeps the prazo's stored value, a non-nil one overrides it (a partial patch).
type AdjustCommand struct {
	TenantID   string
	UserID     string
	DeadlineID string
	Kind       *string
	Days       *int
	Counting   *Counting
	Doubled    *bool
	// DoubledReason is a pointer so an explicit "" can clear the reason while an absent field
	// keeps the stored one (the two must not collapse to the same write).
	DoubledReason *string
	// AnchorEvent / ManualExtraDays are pointers (merge-over-stored, like the rest): a nil field
	// keeps the prazo's stored value, a present one re-anchors / re-counts the extra days.
	AnchorEvent     *AnchorEvent
	ManualExtraDays *int
}

// UpdateDeadlineAdjustParams is the repo port's input for the ajuste UPDATE — the patched
// fields (already merged over the stored values by the use case) plus the recomputed dates.
// It is a plain struct (not the Deadline aggregate) because the ajuste updates a subset of
// columns and leaves status/source/start_date/rules_version untouched.
type UpdateDeadlineAdjustParams struct {
	DeadlineID    string
	TenantID      string
	Kind          string
	Days          int
	Counting      Counting
	Doubled       bool
	DoubledReason string
	EndDate       time.Time
	// PrazoInterno is the recomputed internal safety buffer (internalBufferBusinessDays
	// business days before EndDate), kept in sync with the recomputed EndDate.
	PrazoInterno    time.Time
	HolidaysApplied []time.Time
	// StartDate is re-persisted because re-anchoring changes the start; AnchorEvent /
	// ManualExtraDays are the merged panel choices.
	StartDate       time.Time
	AnchorEvent     AnchorEvent
	ManualExtraDays int
}

// AdjustedDeadline is the recomputed prazo the ajuste returns (and deadline.updated carries):
// the merged, recomputed fact. Status is echoed unchanged (the ajuste never flips the
// lifecycle), so a PENDING prazo stays PENDING and an OPEN one stays OPEN.
type AdjustedDeadline struct {
	ID              string
	CourtRecordID   string
	Kind            string
	Days            int
	Counting        Counting
	Doubled         bool
	DoubledReason   string
	Status          Status
	StartDate       time.Time
	EndDate         time.Time
	HolidaysApplied []time.Time
	AnchorEvent     AnchorEvent
	ManualExtraDays int
}

// MarkedDeadline is the outcome of a manual transition (met/missed): the prazo id and its new
// status. The handler renders it as the response; the use case has already emitted the
// matching deadline.met / deadline.missed event in the same tx.
type MarkedDeadline struct {
	ID     string
	Status Status
}

// Adjust is the F2 ajuste manual (docs/erd-prazos.md §9, PATCH /v1/prazos/:id): in ONE
// tenant-scoped tx it merges the partial patch over the prazo's stored {kind, days, counting,
// doubled, doubled_reason}, RECOMPUTES end_date + holidays_applied from the FIXED start_date,
// UPDATEs the row, and emits deadline.updated — the write and the outbox row committing
// together (transactional outbox).
//
// Only an ACTIVE prazo is adjustable: PENDING (a suggestion, still tunable before confirm) or
// OPEN (confirmed, still correctable). A MET/MISSED/CANCELLED prazo is closed — its dates are
// frozen, so the ajuste is refused with ErrDeadlineNotAdjustable (→ 409), a distinct signal
// from the 404 miss.
//
// Steps (§9):
//  1. load the prazo's full adjustable state by id (a miss → ErrDeadlineNotFound → 404);
//  2. gate on the status (PENDING/OPEN only, else ErrDeadlineNotAdjustable → 409);
//  3. merge the partial patch: each nil field keeps the stored value, each present one wins;
//  4. read the record's court and derive the UF (pkg/tribunal.UF) for the recompute;
//  5. RECOMPUTE end_date + holidays from the FIXED start_date with effectiveDays(days, doubled)
//     via the chosen lib/calendar motor (BUSINESS→dias úteis, CALENDAR→dias corridos);
//  6. UpdateDeadlineAdjust (status left as-is — the ajuste never changes the lifecycle);
//  7. emit deadline.updated in the SAME tx.
//
// TenantID scopes the tx's RLS and every read/write (barrier 1 + 2); it comes from the
// verified principal, never the body.
func (uc *UseCase) Adjust(ctx context.Context, cmd AdjustCommand) (AdjustedDeadline, error) {
	var result AdjustedDeadline
	err := uc.uow.Do(ctx, cmd.TenantID, func(tx database.Tx) error {
		cur, err := uc.repo.GetDeadlineForAdjust(ctx, tx, cmd.DeadlineID, cmd.TenantID)
		if err != nil {
			return err
		}
		if cur.Status != StatusPending && cur.Status != StatusOpen {
			return ErrDeadlineNotAdjustable
		}

		// Merge the partial patch over the stored values: an absent (nil) field keeps the
		// prazo's current value, a present one overrides it.
		kind := cur.Kind
		if cmd.Kind != nil {
			kind = *cmd.Kind
		}
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
		doubledReason := cur.DoubledReason
		if cmd.DoubledReason != nil {
			doubledReason = *cmd.DoubledReason
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
			return err
		}
		uf := tribunal.UF(court)

		// Re-anchor the start on the merged termo inicial (DEADLINE_START keeps the stored anchor),
		// then recompute with the merged extra days through the SAME motor.
		start, err := uc.resolveStart(ctx, tx, cur.IntimationID, cmd.TenantID, anchorEvent, cur.StartDate)
		if err != nil {
			return err
		}

		endDate, holidays, err := uc.computeWithExtra(ctx, counting, start, days, doubled, manualExtraDays, uf, court)
		if err != nil {
			return err
		}
		// Belt-and-suspenders on safety-critical data (mirrors confirm): the recompute always
		// lands after the start (days > 0 is validated at the edge), but never persist an
		// impossible prazo silently.
		if !endDate.After(start) {
			return apperr.NewInvalid("deadline end date must be after start date")
		}

		// V1: recompute the internal safety buffer in lockstep with EndDate, via the SAME
		// uf/court already resolved above — never left stale against the recomputed EndDate.
		prazoInterno, _, err := uc.cal.SubtractBusinessDays(ctx, endDate, internalBufferBusinessDays, uf, court)
		if err != nil {
			return err
		}

		deadlineID, courtRecordID, err := uc.repo.UpdateDeadlineAdjust(ctx, tx, UpdateDeadlineAdjustParams{
			DeadlineID:      cmd.DeadlineID,
			TenantID:        cmd.TenantID,
			Kind:            kind,
			Days:            days,
			Counting:        counting,
			Doubled:         doubled,
			DoubledReason:   doubledReason,
			EndDate:         endDate,
			PrazoInterno:    prazoInterno,
			HolidaysApplied: holidays,
			StartDate:       start,
			AnchorEvent:     anchorEvent,
			ManualExtraDays: manualExtraDays,
		})
		if err != nil {
			return err
		}

		result = AdjustedDeadline{
			ID:              deadlineID,
			CourtRecordID:   courtRecordID,
			Kind:            kind,
			Days:            days,
			Counting:        counting,
			Doubled:         doubled,
			DoubledReason:   doubledReason,
			Status:          cur.Status, // unchanged by the ajuste
			StartDate:       start,
			EndDate:         endDate,
			HolidaysApplied: holidays,
			AnchorEvent:     anchorEvent,
			ManualExtraDays: manualExtraDays,
		}
		return uc.outbox.Publish(ctx, tx, newDeadlineUpdatedFromAdjust(result))
	})
	if err != nil {
		return AdjustedDeadline{}, err
	}
	return result, nil
}

// MarkMet is the manual "marcar cumprido" (docs/erd-prazos.md §9, POST /v1/prazos/:id/met):
// OPEN→MET in ONE tenant-scoped tx, emitting deadline.met. It is the positive counterpart of
// MarkMissed and shares its shape.
func (uc *UseCase) MarkMet(ctx context.Context, tenantID, deadlineID string) (MarkedDeadline, error) {
	return uc.markStatus(ctx, tenantID, deadlineID, StatusMet)
}

// MarkMissed is the manual "marcar perdido" (docs/erd-prazos.md §9, POST /v1/prazos/:id/missed):
// OPEN→MISSED in ONE tenant-scoped tx, emitting deadline.missed. It REUSES the same
// deadline.missed event the D+1 carência auto-miss emits (4b-ii) — the manual and the
// automatic loss are the same fact, so there is one event type, not a parallel one.
func (uc *UseCase) MarkMissed(ctx context.Context, tenantID, deadlineID string) (MarkedDeadline, error) {
	return uc.markStatus(ctx, tenantID, deadlineID, StatusMissed)
}

// markStatus is the shared manual-transition path behind MarkMet/MarkMissed: both flip a prazo
// OPEN→<target> and emit the matching immediate fact. The guard is intentionally strict — only
// OPEN transitions (a PENDING suggestion must be confirmed first; a terminal prazo cannot
// transition again):
//  1. load the prazo's status (a miss → ErrDeadlineNotFound → 404);
//  2. it must be OPEN, else ErrDeadlineNotOpen (→ 409, distinct from the 404 miss);
//  3. MarkDeadlineStatus OPEN→target (the `status = OPEN` guard is the concurrency floor);
//  4. emit the target's immediate fact (deadline.met / deadline.missed) in the SAME tx.
//
// TenantID comes from the verified principal and scopes the tx's RLS (barrier 1 + 2).
func (uc *UseCase) markStatus(ctx context.Context, tenantID, deadlineID string, target Status) (MarkedDeadline, error) {
	var result MarkedDeadline
	err := uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		cur, err := uc.repo.GetDeadlineForCheck(ctx, tx, deadlineID, tenantID)
		if err != nil {
			return err
		}
		if cur.Status != StatusOpen {
			return ErrDeadlineNotOpen
		}

		id, err := uc.repo.MarkDeadlineStatus(ctx, tx, deadlineID, tenantID, StatusOpen, target)
		if err != nil {
			return err
		}

		if err := uc.outbox.Publish(ctx, tx, newTransitionEvent(target, tenantID, id)); err != nil {
			return err
		}
		result = MarkedDeadline{ID: id, Status: target}
		return nil
	})
	if err != nil {
		return MarkedDeadline{}, err
	}
	return result, nil
}

// newTransitionEvent picks the immediate fact for a manual transition: deadline.met for MET,
// deadline.missed for MISSED (the latter REUSED from the 4b-ii carência path). Only these two
// targets reach here (markStatus is the sole caller, with the two Mark* wrappers), so the
// default is defensive.
func newTransitionEvent(target Status, tenantID, deadlineID string) events.Event {
	if target == StatusMet {
		return newDeadlineMet(tenantID, deadlineID)
	}
	return newDeadlineMissed(tenantID, deadlineID)
}

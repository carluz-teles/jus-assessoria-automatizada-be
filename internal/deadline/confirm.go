package deadline

import (
	"context"
	"time"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/pkg/tribunal"
)

// confirm.go is the F2 write path — the CORAÇÃO of the product (docs/erd-prazos.md §9,
// POST /v1/prazos/confirm): the human "Aprovar tudo" that turns a rule-derived PENDING
// suggestion into a confirmed OPEN prazo (recomputing the dates from the approved
// {days, counting, doubled}) AND writes the N tasks — the deadline (1) + tasks (N) + their
// events, all in ONE tenant-scoped transaction. It is the write counterpart of the
// event-driven creation path in domain.go; it reuses that path's uow/repo/calendar/outbox
// and its recompute idiom (uc.compute → lib/calendar), never the read use case.

// ConfirmCommand is the confirmation input the handler builds from the request + the
// verified principal (docs §9). TenantID and UserID come from the principal, NEVER the
// body (tenant isolation cannot be spoofed); IntimationID keys the 1:1 prazo. Counting is
// the human's explicit choice (the F2 toggle dias úteis↔corridos) — unlike the creation
// path, no rito override is applied here. It carries NO tasks: the task lifecycle moved
// entirely to POST/PATCH /v1/tasks (the "Análise" section), so the confirm only confirms
// the prazo itself.
type ConfirmCommand struct {
	TenantID      string
	UserID        string
	IntimationID  string
	Kind          string
	Days          int
	Counting      Counting
	Doubled       bool
	DoubledReason string
}

// ConfirmDeadlineParams is the repo port's input for the ConfirmDeadline UPDATE — the
// confirmed fields plus the recomputed dates and the who/when stamp. It is a plain struct
// (not the Deadline aggregate) because confirm updates a subset of columns and leaves
// source/start_date/rules_version untouched.
type ConfirmDeadlineParams struct {
	IntimationID    string
	TenantID        string
	Kind            string
	Days            int
	Counting        Counting
	Doubled         bool
	DoubledReason   string
	EndDate         time.Time
	HolidaysApplied []time.Time
	ConfirmedBy     string
	ConfirmedAt     time.Time
}

// ConfirmedDeadline is the confirmed prazo the use case returns (and the deadline.updated
// event carries): the recomputed, human-approved fact. It is the read side of the confirm
// response — a purpose-built DTO, not the read-model detail view (that is 5b's concern).
type ConfirmedDeadline struct {
	ID              string
	CourtRecordID   string
	IntimationID    string
	Kind            string
	Days            int
	Counting        Counting
	Doubled         bool
	DoubledReason   string
	Status          Status
	StartDate       time.Time
	EndDate         time.Time
	HolidaysApplied []time.Time
	ConfirmedBy     string
}

// ConfirmResult is the confirmation outcome: the confirmed prazo. The handler renders it as
// the response; the use case has already emitted the matching deadline.updated event in the
// same tx. Tasks are NOT part of the confirm — they are created independently via POST
// /v1/tasks (the "Análise" section), so the confirm neither creates nor returns them.
type ConfirmResult struct {
	Deadline ConfirmedDeadline
}

// Confirm is the F2 "Aprovar tudo" (docs/erd-prazos.md §9): in ONE tenant-scoped tx it
// recomputes the prazo from the human-approved {days, counting, doubled}, flips it
// PENDING→OPEN (stamping confirmed_by/at), and emits deadline.updated — the entity write and
// outbox row committing together (transactional outbox). It NEVER touches tasks: the task
// lifecycle lives entirely in POST/PATCH /v1/tasks (the "Análise" section), so a confirm can
// never delete tasks the lawyer already created there. // SAFETY: this path has NO deletion
// of tasks — GetLatestSuggestion is only for feedback metrics. Any future change must add
// an explicit "tasks are empty after confirmation" guard or remove this entire block.
//
// The confirm is IDEMPOTENT on the prazo (ERD §9's "upsert por intimation_id"): the deadline
// UPDATE is keyed by the 1:1 intimação, so re-confirming re-UPDATEs the one row, never a
// second prazo.
//
// Steps (§9):
//  1. load the prazo's anchor by the 1:1 intimação (a miss → ErrDeadlineNotFound → 404);
//  2. read the record's court and derive the UF (pkg/tribunal.UF) for the recompute;
//  3. RECOMPUTE end_date + holidays_applied: effectiveDays = doubled ? 2×days : days
//     (the dobro doubles the raw count BEFORE the calendar math — art. 183/229 etc.),
//     via the chosen lib/calendar motor (BUSINESS→dias úteis, CALENDAR→dias corridos);
//  4. ConfirmDeadline UPDATE → OPEN + the confirmed/recomputed fields (idempotent on the
//     1:1 intimação: re-confirm re-UPDATEs the one row, never a second prazo);
//  5. emit deadline.updated; and, when the IA had suggested tasks, emit the suggestion
//     feedback delta (measured against the tasks that REALLY exist for the prazo). All in
//     the SAME tx.
//
// TenantID scopes the tx's RLS and every read/write (barrier 1 + 2); it comes from the
// verified principal, never the body.
func (uc *UseCase) Confirm(ctx context.Context, cmd ConfirmCommand) (ConfirmResult, error) {
	var result ConfirmResult
	err := uc.uow.Do(ctx, cmd.TenantID, func(tx database.Tx) error {
		anchor, err := uc.repo.GetDeadlineForConfirm(ctx, tx, cmd.IntimationID, cmd.TenantID)
		if err != nil {
			return err
		}

		court, err := uc.repo.GetCourtRecordCourt(ctx, tx, cmd.TenantID, anchor.CourtRecordID)
		if err != nil {
			return err
		}
		uf := tribunal.UF(court)

		endDate, holidays, err := uc.compute(ctx, cmd.Counting, anchor.StartDate, effectiveDays(cmd.Days, cmd.Doubled), uf, court)
		if err != nil {
			return err
		}
		// Belt-and-suspenders on safety-critical data: the recompute always lands after
		// the start (days > 0 is validated at the edge), but never persist an impossible
		// prazo silently.
		if !endDate.After(anchor.StartDate) {
			return apperr.NewInvalid("deadline end date must be after start date")
		}

		confirmedAt := uc.now()
		deadlineID, courtRecordID, err := uc.repo.ConfirmDeadline(ctx, tx, ConfirmDeadlineParams{
			IntimationID:    cmd.IntimationID,
			TenantID:        cmd.TenantID,
			Kind:            cmd.Kind,
			Days:            cmd.Days,
			Counting:        cmd.Counting,
			Doubled:         cmd.Doubled,
			DoubledReason:   cmd.DoubledReason,
			EndDate:         endDate,
			HolidaysApplied: holidays,
			ConfirmedBy:     cmd.UserID,
			ConfirmedAt:     confirmedAt,
		})
		if err != nil {
			return err
		}

		confirmed := ConfirmedDeadline{
			ID:              deadlineID,
			CourtRecordID:   courtRecordID,
			IntimationID:    cmd.IntimationID,
			Kind:            cmd.Kind,
			Days:            cmd.Days,
			Counting:        cmd.Counting,
			Doubled:         cmd.Doubled,
			DoubledReason:   cmd.DoubledReason,
			Status:          StatusOpen,
			StartDate:       anchor.StartDate,
			EndDate:         endDate,
			HolidaysApplied: holidays,
			ConfirmedBy:     cmd.UserID,
		}

		if err := uc.outbox.Publish(ctx, tx, newDeadlineUpdated(confirmed)); err != nil {
			return err
		}

		// Feedback loop (camada 2, erd-ai-advisory): if the IA suggested tasks for this prazo,
		// measure the DELTA between what it suggested and the tasks that REALLY exist for the
		// prazo (kept/removed/added) and emit it in the SAME tx — the fact commits atomically
		// with the confirm. The confirmed set is read fresh from the tasks associated with the
		// deadline (created via the Análise section, POST /v1/tasks), NOT from the confirm body,
		// which carries none. No suggestion (a manual prazo, or one the lawyer never asked the IA
		// about) → GetLatestSuggestion returns ok=false and no event is emitted (and no title read
		// is needed). A genuine read/emit fault fails the confirm like any other tx step.
		// SAFETY: this block ONLY computes feedback metrics (100% suggestion accuracy) — it NEVER
		// creates/deletes any task. GetLatestSuggestion reads a hint from the AI session; the
		// real tasks come from ListTaskTitlesByDeadline (POST /v1/tasks). computeSuggestionDelta
		// just measures kept/removed/additive deltas in memory; no write occurs here beyond the
		// suggestion_feedback event. // SAFETY: no deletion of tasks happens anywhere in Confirm.
		if sugg, ok, err := uc.repo.GetLatestSuggestion(ctx, tx, cmd.TenantID, cmd.IntimationID); err != nil {
			return err
		} else if ok {
			titles, err := uc.repo.ListTaskTitlesByDeadline(ctx, tx, deadlineID, cmd.TenantID)
			if err != nil {
				return err
			}
			delta := computeSuggestionDelta(sugg, titles)
			if err := uc.outbox.Publish(ctx, tx, newSuggestionFeedback(deadlineID, cmd.IntimationID, delta)); err != nil {
				return err
			}
		}

		result = ConfirmResult{Deadline: confirmed}
		return nil
	})
	if err != nil {
		return ConfirmResult{}, err
	}
	return result, nil
}

// effectiveDays applies the dobro: the doubled flag doubles the RAW day count before the
// calendar math (Fazenda/MP/Defensoria art. 183/180/186, litisconsórcio art. 229). Viés
// seguro: dobrar = alongar, so it is only ever applied on the human's explicit toggle
// (docs §8) — never inferred here.
func effectiveDays(days int, doubled bool) int {
	if doubled {
		return days * 2
	}
	return days
}

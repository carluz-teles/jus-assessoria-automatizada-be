package deadline

import (
	"context"
	"fmt"
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
// path, no rito override is applied here.
type ConfirmCommand struct {
	TenantID      string
	UserID        string
	IntimationID  string
	Kind          string
	Days          int
	Counting      Counting
	Doubled       bool
	DoubledReason string
	Tasks         []ConfirmTaskInput
}

// ConfirmTaskInput is one action item the F2 form submits. Only Title is required; Kind,
// Description, DueDate and AssigneeUserID are optional (a task can be undated/unassigned).
type ConfirmTaskInput struct {
	Title          string
	Kind           string
	Description    string
	DueDate        *time.Time
	AssigneeUserID string
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

// ConfirmedTask is one created task the use case returns (and task.created carries), with
// its DB-assigned id.
type ConfirmedTask struct {
	ID             string
	DeadlineID     string
	CourtRecordID  string
	IntimationID   string
	Title          string
	Description    string
	Kind           string
	DueDate        *time.Time
	Status         TaskStatus
	Source         Source
	AssigneeUserID string
	CreatedBy      string
}

// ConfirmResult is the whole confirmation outcome: the confirmed prazo + the created
// tasks. The handler renders it as the response; the use case has already emitted the
// matching deadline.updated + task.created events in the same tx.
type ConfirmResult struct {
	Deadline ConfirmedDeadline
	Tasks    []ConfirmedTask
}

// Confirm is the F2 "Aprovar tudo" (docs/erd-prazos.md §9): in ONE tenant-scoped tx it
// recomputes the prazo from the human-approved {days, counting, doubled}, flips it
// PENDING→OPEN (stamping confirmed_by/at), REPLACES the prazo's tasks with the submitted N,
// and emits deadline.updated + one task.created per task — entity writes and outbox rows
// committing together (transactional outbox).
//
// The confirm is IDEMPOTENT on the whole prazo (ERD §9's "upsert por intimation_id"), on
// BOTH the deadline and its tasks: the deadline UPDATE is keyed by the 1:1 intimação (never a
// second prazo), and the tasks follow REPLACE semantics — every confirm deletes the prazo's
// tasks before re-inserting the submitted set, so re-confirming leaves EXACTLY the last
// submit (an empty task set clears them) instead of accumulating +N rows each call.
//
// Steps (§9):
//  1. load the prazo's anchor by the 1:1 intimação (a miss → ErrDeadlineNotFound → 404);
//  2. read the record's court and derive the UF (pkg/tribunal.UF) for the recompute;
//  3. RECOMPUTE end_date + holidays_applied: effectiveDays = doubled ? 2×days : days
//     (the dobro doubles the raw count BEFORE the calendar math — art. 183/229 etc.),
//     via the chosen lib/calendar motor (BUSINESS→dias úteis, CALENDAR→dias corridos);
//  4. ConfirmDeadline UPDATE → OPEN + the confirmed/recomputed fields (idempotent on the
//     1:1 intimação: re-confirm re-UPDATEs the one row, never a second prazo);
//  5. DeleteTasksByDeadline (REPLACE): drop the prazo's existing tasks so the confirm is
//     idempotent on tasks too — re-confirm does not accumulate;
//  6. InsertTask per submitted item (OPEN, MANUAL, created_by = principal) + emit task.created;
//  7. emit deadline.updated. All in the SAME tx.
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
		// The tasks' own dates cannot fall past the legal prazo (ERD §4: due_date ≤
		// deadline.end_date). end_date is only known here (recomputed in the tx), so the
		// cross-field check lives in the use case, not the edge validator.
		if err := checkTaskDueDates(cmd.Tasks, endDate); err != nil {
			return err
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

		// REPLACE, not append: drop this prazo's existing tasks before re-inserting the
		// submitted set, so re-confirming the same intimação leaves EXACTLY the last submit
		// (ERD §9's "upsert idempotente por intimation_id") instead of accumulating +N rows
		// each call. Same tx as the confirm — the delete + re-insert commit atomically.
		if err := uc.repo.DeleteTasksByDeadline(ctx, tx, deadlineID, cmd.TenantID); err != nil {
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

		tasks := make([]ConfirmedTask, 0, len(cmd.Tasks))
		for _, in := range cmd.Tasks {
			saved, err := uc.repo.InsertTask(ctx, tx, &Task{
				TenantID:       cmd.TenantID,
				CourtRecordID:  courtRecordID,
				DeadlineID:     deadlineID,
				IntimationID:   cmd.IntimationID,
				Title:          in.Title,
				Description:    in.Description,
				Kind:           in.Kind,
				DueDate:        in.DueDate,
				Status:         TaskStatusOpen,
				Source:         SourceManual,
				AssigneeUserID: in.AssigneeUserID,
				CreatedBy:      cmd.UserID,
			})
			if err != nil {
				return err
			}

			ct := ConfirmedTask{
				ID:             saved.ID,
				DeadlineID:     deadlineID,
				CourtRecordID:  courtRecordID,
				IntimationID:   cmd.IntimationID,
				Title:          saved.Title,
				Description:    saved.Description,
				Kind:           saved.Kind,
				DueDate:        saved.DueDate,
				Status:         saved.Status,
				Source:         saved.Source,
				AssigneeUserID: saved.AssigneeUserID,
				CreatedBy:      saved.CreatedBy,
			}
			if err := uc.outbox.Publish(ctx, tx, newTaskCreated(ct)); err != nil {
				return err
			}
			tasks = append(tasks, ct)
		}

		if err := uc.outbox.Publish(ctx, tx, newDeadlineUpdated(confirmed)); err != nil {
			return err
		}

		// TODO(checklist-template): seed a default checklist (Ler intimação, Analisar documentos,
		// Definir estratégia, Redigir, Revisar, Aprovar, Protocolar, Conferir) on the created tasks
		// when kind is MANIFESTACAO/peça. DELIBERATELY NOT DONE here: the confirm has REPLACE (upsert)
		// semantics on re-confirm — auto-seeding items would either duplicate on every re-confirm or
		// need a "seed only on first confirm" guard, both of which complicate the coração do produto.
		// The FE can offer the template via POST /v1/tasks/:id/items after confirm (low risk, explicit).

		result = ConfirmResult{Deadline: confirmed, Tasks: tasks}
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

// checkTaskDueDates enforces ERD §4's task invariant: a task's own due_date cannot fall
// after the legal prazo's end_date. A violation is a KindInvalid (→ 400), reported with
// the offending value so the F2 UI can point at the field.
func checkTaskDueDates(tasks []ConfirmTaskInput, endDate time.Time) error {
	for _, t := range tasks {
		if t.DueDate != nil && t.DueDate.After(endDate) {
			return apperr.NewInvalid(fmt.Sprintf("task due_date %s is after the deadline end_date %s",
				t.DueDate.Format(time.DateOnly), endDate.Format(time.DateOnly)))
		}
	}
	return nil
}

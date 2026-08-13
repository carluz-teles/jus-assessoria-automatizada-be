package deadline

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jusassessoria/platform/internal/deadline/deadlinedb"
	"github.com/jusassessoria/platform/lib/database"
)

// pgRepository is the sqlc-backed Repository. Every method binds the generated code to
// the caller's tx (all reads and the write are transactional, so RLS scopes them to the
// event's tenant); the repo holds no pool of its own — the use case owns the boundary.
type pgRepository struct{}

var _ Repository = (*pgRepository)(nil)

// NewRepository returns the Repository. It is stateless: each method binds deadlinedb to
// the tx it is given, so there is nothing to inject at construction.
func NewRepository() Repository { return &pgRepository{} }

// GetCourtRecordClass reads the rito signal for the record inside the caller's tx,
// filtered by tenantID (barrier 1). A missing record — or one belonging to another
// tenant — maps to the typed ErrCourtRecordNotFound (never nil, nil); a present record
// with a NULL class returns "".
func (r *pgRepository) GetCourtRecordClass(ctx context.Context, tx database.Tx, tenantID, courtRecordID string) (string, error) {
	id, err := parseUUID(courtRecordID)
	if err != nil {
		return "", err
	}
	tenant, err := parseUUID(tenantID)
	if err != nil {
		return "", err
	}

	class, err := deadlinedb.New(tx).GetCourtRecordClass(ctx, deadlinedb.GetCourtRecordClassParams{
		ID:       id,
		TenantID: tenant,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrCourtRecordNotFound
	}
	if err != nil {
		return "", database.WrapInfra(err)
	}
	return derefString(class), nil
}

// ResolveRule resolves the most specific active rule for (intimationType, court) inside
// the caller's tx (the '*' catch-all is the floor, so an unknown type still resolves).
// No row at all maps to ErrRuleNotFound (a missing seed — a config fault).
func (r *pgRepository) ResolveRule(ctx context.Context, tx database.Tx, rulesVersion, intimationType, court string) (DeadlineRule, error) {
	// court is passed as the LIKE subject ($3); the generated param is named CourtPrefix
	// (sqlc infers it from the nullable court_prefix column it is compared against), but
	// the value IS the record's court sigla. It is always non-empty from the event.
	courtArg := court
	row, err := deadlinedb.New(tx).ResolveDeadlineRule(ctx, deadlinedb.ResolveDeadlineRuleParams{
		RulesVersion:   rulesVersion,
		IntimationType: intimationType,
		CourtPrefix:    &courtArg,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return DeadlineRule{}, ErrRuleNotFound
	}
	if err != nil {
		return DeadlineRule{}, database.WrapInfra(err)
	}

	return DeadlineRule{
		RulesVersion: row.RulesVersion,
		Kind:         row.Kind,
		Days:         int(row.Days),
		Counting:     Counting(row.Counting),
		Doubled:      row.Doubled,
	}, nil
}

// InsertDeadline persists the derived prazo inside the caller's tx and returns it with
// its DB-assigned id. The insert is idempotent on the 1:1 notification_id (ON CONFLICT
// DO NOTHING): a re-derivation yields no row (pgx.ErrNoRows), mapped to ErrDeadlineExists
// so the use case no-ops instead of opening a phantom prazo. IntimationID is written to
// the notification_id column (the historic-name FK to intimation — see mapper.go).
func (r *pgRepository) InsertDeadline(ctx context.Context, tx database.Tx, d *Deadline) (*Deadline, error) {
	tenantID, err := parseUUID(d.TenantID)
	if err != nil {
		return nil, err
	}
	courtRecordID, err := parseUUID(d.CourtRecordID)
	if err != nil {
		return nil, err
	}
	intimationID, err := parseUUID(d.IntimationID)
	if err != nil {
		return nil, err
	}
	holidays, err := marshalHolidays(d.HolidaysApplied)
	if err != nil {
		return nil, err
	}

	id, err := deadlinedb.New(tx).InsertDeadline(ctx, deadlinedb.InsertDeadlineParams{
		TenantID:        tenantID,
		CourtRecordID:   courtRecordID,
		NotificationID:  intimationID,
		StartDate:       pgDate(d.StartDate),
		EndDate:         pgDate(d.EndDate),
		Days:            int32(d.Days),
		Counting:        string(d.Counting),
		Doubled:         d.Doubled,
		DoubledReason:   textToNull(d.DoubledReason),
		HolidaysApplied: holidays,
		Status:          string(d.Status),
		Source:          string(d.Source),
		Kind:            textToNull(d.Kind),
		RulesVersion:    d.RulesVersion,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrDeadlineExists
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}

	saved := *d
	saved.ID = id.String()
	return &saved, nil
}

// GetDeadlineForConfirm loads the F2 confirmation anchor by the 1:1 notification_id
// (=intimation id) inside the caller's tx, filtered by tenantID (barrier 1). A missing
// prazo — or one in another tenant — maps to the typed ErrDeadlineNotFound (never nil,
// nil). The mapper absorbs the driver types (uuid.UUID, pgtype.Date) so the use case sees
// a pure *DeadlineForConfirm. IntimationID is matched against the notification_id column
// (the historic-name FK to intimation — see mapper.go).
func (r *pgRepository) GetDeadlineForConfirm(ctx context.Context, tx database.Tx, intimationID, tenantID string) (*DeadlineForConfirm, error) {
	intID, err := parseUUID(intimationID)
	if err != nil {
		return nil, err
	}
	tenant, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}

	row, err := deadlinedb.New(tx).GetDeadlineForConfirm(ctx, deadlinedb.GetDeadlineForConfirmParams{
		NotificationID: intID,
		TenantID:       tenant,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrDeadlineNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}

	return &DeadlineForConfirm{
		ID:            row.ID.String(),
		CourtRecordID: row.CourtRecordID.String(),
		StartDate:     row.StartDate.Time,
	}, nil
}

// GetCourtRecordCourt reads the court sigla for the record inside the caller's tx,
// filtered by tenantID (barrier 1) — the confirm counterpart of GetCourtRecordClass. A
// missing record — or one belonging to another tenant — maps to the typed
// ErrCourtRecordNotFound (never nil, nil); court is NOT NULL so a present record always
// yields a non-empty sigla.
func (r *pgRepository) GetCourtRecordCourt(ctx context.Context, tx database.Tx, tenantID, courtRecordID string) (string, error) {
	id, err := parseUUID(courtRecordID)
	if err != nil {
		return "", err
	}
	tenant, err := parseUUID(tenantID)
	if err != nil {
		return "", err
	}

	court, err := deadlinedb.New(tx).GetCourtRecordCourt(ctx, deadlinedb.GetCourtRecordCourtParams{
		ID:       id,
		TenantID: tenant,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrCourtRecordNotFound
	}
	if err != nil {
		return "", database.WrapInfra(err)
	}
	return court, nil
}

// ConfirmDeadline flips the prazo PENDING→OPEN with the confirmed fields + recomputed
// dates inside the caller's tx, keyed by the 1:1 notification_id (=intimation id) and
// filtered by tenantID (barrier 1). A no-match (no prazo for the intimação) yields
// pgx.ErrNoRows, mapped to the typed ErrDeadlineNotFound (→ 404) so the use case aborts
// the tx cleanly instead of confirming nothing. On a hit it returns the confirmed prazo's
// id and the record it hangs on. confirmed_by is always set (the principal), so it is a
// required uuid; the mapper writes the recomputed dates and the jsonb holidays audit.
func (r *pgRepository) ConfirmDeadline(ctx context.Context, tx database.Tx, p ConfirmDeadlineParams) (string, string, error) {
	intID, err := parseUUID(p.IntimationID)
	if err != nil {
		return "", "", err
	}
	tenant, err := parseUUID(p.TenantID)
	if err != nil {
		return "", "", err
	}
	confirmedBy, err := pgUUID(p.ConfirmedBy)
	if err != nil {
		return "", "", err
	}
	holidays, err := marshalHolidays(p.HolidaysApplied)
	if err != nil {
		return "", "", err
	}

	row, err := deadlinedb.New(tx).ConfirmDeadline(ctx, deadlinedb.ConfirmDeadlineParams{
		NotificationID:  intID,
		TenantID:        tenant,
		Kind:            textToNull(p.Kind),
		Days:            int32(p.Days),
		Counting:        string(p.Counting),
		Doubled:         p.Doubled,
		DoubledReason:   textToNull(p.DoubledReason),
		EndDate:         pgDate(p.EndDate),
		HolidaysApplied: holidays,
		ConfirmedBy:     confirmedBy,
		ConfirmedAt:     pgTimestamptz(p.ConfirmedAt),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrDeadlineNotFound
	}
	if err != nil {
		return "", "", database.WrapInfra(err)
	}
	return row.ID.String(), row.CourtRecordID.String(), nil
}

// DeleteTasksByDeadline drops the confirmed prazo's tasks inside the caller's tx, scoped to
// (deadlineID, tenantID) (barrier 1 + RLS barrier 2). It is the REPLACE step of the F2
// confirm: the use case runs it right after ConfirmDeadline and before the InsertTask loop,
// so re-confirming the same intimação leaves only the last submit's tasks (ERD §9's upsert
// semantics) instead of accumulating +N rows. deadline_id is a nullable column (mapper lifts
// it to pgtype.UUID); tenant_id is NOT NULL. Deleting no row (a first confirm) is a clean
// success, not an error.
func (r *pgRepository) DeleteTasksByDeadline(ctx context.Context, tx database.Tx, deadlineID, tenantID string) error {
	id, err := pgUUID(deadlineID)
	if err != nil {
		return err
	}
	tenant, err := parseUUID(tenantID)
	if err != nil {
		return err
	}

	if err := deadlinedb.New(tx).DeleteTasksByDeadline(ctx, deadlinedb.DeleteTasksByDeadlineParams{
		DeadlineID: id,
		TenantID:   tenant,
	}); err != nil {
		return database.WrapInfra(err)
	}
	return nil
}

// InsertTask persists one task inside the caller's tx and returns it with its DB-assigned id
// (echoing the entity, like InsertDeadline). tenant_id is NOT NULL; the context FKs
// (court_record_id/deadline_id/intimation_id) map to NULLABLE columns and are OPTIONAL — the F2
// confirm always fills them (the prazo's context), but a manual avulsa task (POST /v1/tasks) may
// leave them empty, so the mapper lifts each via pgOptionalUUID ("" → NULL). created_by is
// always the principal on both paths; assignee_user_id/due_date/description/kind are optional too.
func (r *pgRepository) InsertTask(ctx context.Context, tx database.Tx, t *Task) (*Task, error) {
	tenant, err := parseUUID(t.TenantID)
	if err != nil {
		return nil, err
	}
	courtRecordID, err := pgOptionalUUID(t.CourtRecordID)
	if err != nil {
		return nil, err
	}
	deadlineID, err := pgOptionalUUID(t.DeadlineID)
	if err != nil {
		return nil, err
	}
	intimationID, err := pgOptionalUUID(t.IntimationID)
	if err != nil {
		return nil, err
	}
	assignee, err := pgOptionalUUID(t.AssigneeUserID)
	if err != nil {
		return nil, err
	}
	createdBy, err := pgUUID(t.CreatedBy)
	if err != nil {
		return nil, err
	}

	id, err := deadlinedb.New(tx).InsertTask(ctx, deadlinedb.InsertTaskParams{
		TenantID:       tenant,
		CourtRecordID:  courtRecordID,
		DeadlineID:     deadlineID,
		IntimationID:   intimationID,
		Title:          t.Title,
		Description:    textToNull(t.Description),
		Kind:           textToNull(t.Kind),
		DueDate:        pgOptionalDate(t.DueDate),
		Status:         string(t.Status),
		Source:         string(t.Source),
		AssigneeUserID: assignee,
		CreatedBy:      createdBy,
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}

	saved := *t
	saved.ID = id.String()
	return &saved, nil
}

// GetTaskForUpdate loads a task's editable state by its id inside the caller's tx, filtered
// by tenantID (barrier 1) — the PATCH /v1/tasks/:id counterpart of GetDeadlineForAdjust. A
// missing id — or one in another tenant — maps to the typed ErrTaskNotFound (never nil, nil).
// The mapper absorbs the driver types (pgtype.Date, pgtype.UUID) and lifts the nullable
// description/kind/assignee to ""/nil so the use case merges over a pure *TaskForUpdate.
func (r *pgRepository) GetTaskForUpdate(ctx context.Context, tx database.Tx, taskID, tenantID string) (*TaskForUpdate, error) {
	id, err := parseUUID(taskID)
	if err != nil {
		return nil, err
	}
	tenant, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}

	row, err := deadlinedb.New(tx).GetTaskForUpdate(ctx, deadlinedb.GetTaskForUpdateParams{ID: id, TenantID: tenant})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrTaskNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}

	return &TaskForUpdate{
		ID:             row.ID.String(),
		Status:         TaskStatus(row.Status),
		Title:          row.Title,
		Description:    derefString(row.Description),
		Kind:           derefString(row.Kind),
		DueDate:        datePtr(row.DueDate),
		AssigneeUserID: uuidText(row.AssigneeUserID),
	}, nil
}

// UpdateTask writes the merged {title, description, kind, due_date, assignee_user_id} inside
// the caller's tx, keyed by the task id and filtered by tenantID (barrier 1). It is the ajuste
// counterpart of ConfirmDeadline. A no-match (the row vanished mid-tx) yields pgx.ErrNoRows,
// mapped to the typed ErrTaskNotFound. On a hit it returns the full saved task (from RETURNING)
// so the handler renders the response without a re-read; the mapper absorbs the driver types.
func (r *pgRepository) UpdateTask(ctx context.Context, tx database.Tx, p UpdateTaskParams) (*Task, error) {
	id, err := parseUUID(p.TaskID)
	if err != nil {
		return nil, err
	}
	tenant, err := parseUUID(p.TenantID)
	if err != nil {
		return nil, err
	}
	assignee, err := pgOptionalUUID(p.AssigneeUserID)
	if err != nil {
		return nil, err
	}

	row, err := deadlinedb.New(tx).UpdateTask(ctx, deadlinedb.UpdateTaskParams{
		ID:             id,
		TenantID:       tenant,
		Title:          p.Title,
		Description:    textToNull(p.Description),
		Kind:           textToNull(p.Kind),
		DueDate:        pgOptionalDate(p.DueDate),
		AssigneeUserID: assignee,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrTaskNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}

	return &Task{
		ID:             row.ID.String(),
		TenantID:       row.TenantID.String(),
		CourtRecordID:  uuidText(row.CourtRecordID),
		DeadlineID:     uuidText(row.DeadlineID),
		IntimationID:   uuidText(row.IntimationID),
		Title:          row.Title,
		Description:    derefString(row.Description),
		Kind:           derefString(row.Kind),
		DueDate:        datePtr(row.DueDate),
		Status:         TaskStatus(row.Status),
		Source:         Source(row.Source),
		AssigneeUserID: uuidText(row.AssigneeUserID),
		CreatedBy:      uuidText(row.CreatedBy),
		CompletedAt:    timestampPtr(row.CompletedAt),
	}, nil
}

// GetTaskForTransition re-reads a task's current status by its id inside the caller's tx,
// filtered by tenantID (barrier 1) — the done/dismiss counterpart of GetDeadlineForCheck. A
// missing id — or one in another tenant — maps to the typed ErrTaskNotFound. The status lets
// the use case distinguish a 404 miss from a 409 invalid transition before the guarded flip.
func (r *pgRepository) GetTaskForTransition(ctx context.Context, tx database.Tx, taskID, tenantID string) (TaskStatus, error) {
	id, err := parseUUID(taskID)
	if err != nil {
		return "", err
	}
	tenant, err := parseUUID(tenantID)
	if err != nil {
		return "", err
	}

	row, err := deadlinedb.New(tx).GetTaskForTransition(ctx, deadlinedb.GetTaskForTransitionParams{ID: id, TenantID: tenant})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrTaskNotFound
	}
	if err != nil {
		return "", database.WrapInfra(err)
	}
	return TaskStatus(row.Status), nil
}

// MarkTaskStatus flips the task from `from` to `to` inside the caller's tx, keyed by its id and
// filtered by tenantID (barrier 1), stamping completed_at (a real time for DONE, NULL for
// DISMISSED). The query's `status = from` guard defends the write against a racing flip: a
// no-match (already transitioned) yields pgx.ErrNoRows, mapped to the typed ErrTaskNotFound. On
// a hit it returns the flipped task's id so task.completed/task.dismissed commits in the same tx.
func (r *pgRepository) MarkTaskStatus(ctx context.Context, tx database.Tx, taskID, tenantID string, from, to TaskStatus, completedAt *time.Time) (string, error) {
	id, err := parseUUID(taskID)
	if err != nil {
		return "", err
	}
	tenant, err := parseUUID(tenantID)
	if err != nil {
		return "", err
	}

	flipped, err := deadlinedb.New(tx).MarkTaskStatus(ctx, deadlinedb.MarkTaskStatusParams{
		NewStatus:     string(to),
		CompletedAt:   pgOptionalTimestamptz(completedAt),
		ID:            id,
		TenantID:      tenant,
		CurrentStatus: string(from),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrTaskNotFound
	}
	if err != nil {
		return "", database.WrapInfra(err)
	}
	return flipped.String(), nil
}

// GetDeadlineForAdjust loads a prazo's full adjustable state by its id inside the caller's
// tx, filtered by tenantID (barrier 1) — the ajuste counterpart of GetDeadlineForConfirm. A
// missing id — or one in another tenant — maps to the typed ErrDeadlineNotFound (never nil,
// nil). The mapper absorbs the driver types (uuid.UUID, pgtype.Date) and lifts the nullable
// kind/doubled_reason to "" so the use case sees a pure *DeadlineForAdjust.
func (r *pgRepository) GetDeadlineForAdjust(ctx context.Context, tx database.Tx, deadlineID, tenantID string) (*DeadlineForAdjust, error) {
	id, err := parseUUID(deadlineID)
	if err != nil {
		return nil, err
	}
	tenant, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}

	row, err := deadlinedb.New(tx).GetDeadlineForAdjust(ctx, deadlinedb.GetDeadlineForAdjustParams{
		ID:       id,
		TenantID: tenant,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrDeadlineNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}

	return &DeadlineForAdjust{
		ID:            row.ID.String(),
		CourtRecordID: row.CourtRecordID.String(),
		StartDate:     row.StartDate.Time,
		Status:        Status(row.Status),
		Kind:          derefString(row.Kind),
		Days:          int(row.Days),
		Counting:      Counting(row.Counting),
		Doubled:       row.Doubled,
		DoubledReason: derefString(row.DoubledReason),
	}, nil
}

// UpdateDeadlineAdjust writes the patched fields + recomputed dates inside the caller's tx,
// keyed by the prazo id and filtered by tenantID (barrier 1). It is the ajuste counterpart of
// ConfirmDeadline; like it, the mapper writes the recomputed date and the jsonb holidays
// audit. A no-match (the row vanished mid-tx) yields pgx.ErrNoRows, mapped to the typed
// ErrDeadlineNotFound. On a hit it returns the prazo id and the record it hangs on.
func (r *pgRepository) UpdateDeadlineAdjust(ctx context.Context, tx database.Tx, p UpdateDeadlineAdjustParams) (string, string, error) {
	id, err := parseUUID(p.DeadlineID)
	if err != nil {
		return "", "", err
	}
	tenant, err := parseUUID(p.TenantID)
	if err != nil {
		return "", "", err
	}
	holidays, err := marshalHolidays(p.HolidaysApplied)
	if err != nil {
		return "", "", err
	}

	row, err := deadlinedb.New(tx).UpdateDeadlineAdjust(ctx, deadlinedb.UpdateDeadlineAdjustParams{
		ID:              id,
		TenantID:        tenant,
		Kind:            textToNull(p.Kind),
		Days:            int32(p.Days),
		Counting:        string(p.Counting),
		Doubled:         p.Doubled,
		DoubledReason:   textToNull(p.DoubledReason),
		EndDate:         pgDate(p.EndDate),
		HolidaysApplied: holidays,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrDeadlineNotFound
	}
	if err != nil {
		return "", "", database.WrapInfra(err)
	}
	return row.ID.String(), row.CourtRecordID.String(), nil
}

// MarkDeadlineStatus flips the prazo from `from` to `to` inside the caller's tx, keyed by its
// id and filtered by tenantID (barrier 1). The query's `status = from` guard defends the
// write against a racing flip: a no-match (already transitioned) yields pgx.ErrNoRows, mapped
// to the typed ErrDeadlineNotFound. On a hit it returns the flipped prazo's id so
// deadline.met/deadline.missed commits in the same tx.
func (r *pgRepository) MarkDeadlineStatus(ctx context.Context, tx database.Tx, deadlineID, tenantID string, from, to Status) (string, error) {
	id, err := parseUUID(deadlineID)
	if err != nil {
		return "", err
	}
	tenant, err := parseUUID(tenantID)
	if err != nil {
		return "", err
	}

	flipped, err := deadlinedb.New(tx).MarkDeadlineStatus(ctx, deadlinedb.MarkDeadlineStatusParams{
		NewStatus:     string(to),
		ID:            id,
		TenantID:      tenant,
		CurrentStatus: string(from),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrDeadlineNotFound
	}
	if err != nil {
		return "", database.WrapInfra(err)
	}
	return flipped.String(), nil
}

// EnsureTaskInTenant confirms the parent task exists in the tenant inside the caller's tx
// (the guard the checklist create runs first). A miss — or a foreign tenant's task — yields
// pgx.ErrNoRows, mapped to the typed ErrTaskItemNotFound (→ 404) so an item is never grafted
// onto a non-existent or foreign task.
func (r *pgRepository) EnsureTaskInTenant(ctx context.Context, tx database.Tx, taskID, tenantID string) error {
	id, err := parseUUID(taskID)
	if err != nil {
		return err
	}
	tenant, err := parseUUID(tenantID)
	if err != nil {
		return err
	}

	_, err = deadlinedb.New(tx).TaskExistsInTenant(ctx, deadlinedb.TaskExistsInTenantParams{ID: id, TenantID: tenant})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrTaskItemNotFound
	}
	if err != nil {
		return database.WrapInfra(err)
	}
	return nil
}

// NextTaskItemPosition returns the append slot (max(position)+1, or 0 when empty) for a task's
// checklist inside the caller's tx, scoped to (taskID, tenantID). The COALESCE means an empty
// checklist never yields NULL, so there is no not-found here.
func (r *pgRepository) NextTaskItemPosition(ctx context.Context, tx database.Tx, taskID, tenantID string) (int, error) {
	id, err := parseUUID(taskID)
	if err != nil {
		return 0, err
	}
	tenant, err := parseUUID(tenantID)
	if err != nil {
		return 0, err
	}

	pos, err := deadlinedb.New(tx).NextTaskItemPosition(ctx, deadlinedb.NextTaskItemPositionParams{TaskID: id, TenantID: tenant})
	if err != nil {
		return 0, database.WrapInfra(err)
	}
	return int(pos), nil
}

// InsertTaskItem persists one checklist item inside the caller's tx and returns it with its
// DB-assigned id (echoing the entity, like InsertTask). tenant_id/task_id are NOT NULL; the item
// is born done=false with done_at NULL (the insert sets neither). The mapper absorbs the driver
// types on the returned row.
func (r *pgRepository) InsertTaskItem(ctx context.Context, tx database.Tx, item *TaskItem) (*TaskItem, error) {
	tenant, err := parseUUID(item.TenantID)
	if err != nil {
		return nil, err
	}
	taskID, err := parseUUID(item.TaskID)
	if err != nil {
		return nil, err
	}

	row, err := deadlinedb.New(tx).InsertTaskItem(ctx, deadlinedb.InsertTaskItemParams{
		TenantID: tenant,
		TaskID:   taskID,
		Title:    item.Title,
		Position: int32(item.Position),
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}

	return &TaskItem{
		ID:        row.ID.String(),
		TenantID:  item.TenantID,
		TaskID:    row.TaskID.String(),
		Title:     row.Title,
		Position:  int(row.Position),
		Done:      row.Done,
		DoneAt:    timestampPtr(row.DoneAt),
		CreatedAt: row.CreatedAt.Time,
	}, nil
}

// GetTaskItemForUpdate loads a checklist item's editable {title, done} by (itemID, taskID)
// inside the caller's tx, filtered by tenantID (barrier 1). A miss — including an item under a
// different task — maps to the typed ErrTaskItemNotFound (never nil, nil).
func (r *pgRepository) GetTaskItemForUpdate(ctx context.Context, tx database.Tx, itemID, taskID, tenantID string) (*TaskItemForUpdate, error) {
	id, err := parseUUID(itemID)
	if err != nil {
		return nil, err
	}
	task, err := parseUUID(taskID)
	if err != nil {
		return nil, err
	}
	tenant, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}

	row, err := deadlinedb.New(tx).GetTaskItemForUpdate(ctx, deadlinedb.GetTaskItemForUpdateParams{
		ID:       id,
		TaskID:   task,
		TenantID: tenant,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrTaskItemNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}

	return &TaskItemForUpdate{
		ID:     row.ID.String(),
		TaskID: row.TaskID.String(),
		Title:  row.Title,
		Done:   row.Done,
	}, nil
}

// UpdateTaskItem writes the merged {title, done, done_at} keyed by (item, task, tenant) inside
// the caller's tx (barrier 1). A no-match (the row vanished mid-tx) yields pgx.ErrNoRows, mapped
// to the typed ErrTaskItemNotFound. On a hit it returns the full saved item (from RETURNING) so
// the handler renders it without a re-read; the mapper absorbs the driver types.
func (r *pgRepository) UpdateTaskItem(ctx context.Context, tx database.Tx, p UpdateTaskItemParams) (*TaskItem, error) {
	id, err := parseUUID(p.ItemID)
	if err != nil {
		return nil, err
	}
	task, err := parseUUID(p.TaskID)
	if err != nil {
		return nil, err
	}
	tenant, err := parseUUID(p.TenantID)
	if err != nil {
		return nil, err
	}

	row, err := deadlinedb.New(tx).UpdateTaskItem(ctx, deadlinedb.UpdateTaskItemParams{
		ID:       id,
		TaskID:   task,
		TenantID: tenant,
		Title:    p.Title,
		Done:     p.Done,
		DoneAt:   pgOptionalTimestamptz(p.DoneAt),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrTaskItemNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}

	return &TaskItem{
		ID:        row.ID.String(),
		TenantID:  p.TenantID,
		TaskID:    row.TaskID.String(),
		Title:     row.Title,
		Position:  int(row.Position),
		Done:      row.Done,
		DoneAt:    timestampPtr(row.DoneAt),
		CreatedAt: row.CreatedAt.Time,
	}, nil
}

// DeleteTaskItem removes one checklist item keyed by (item, task, tenant) inside the caller's tx
// (barrier 1). The RETURNING id means a no-match (foreign/unknown item) yields pgx.ErrNoRows,
// mapped to the typed ErrTaskItemNotFound (→ 404) — never a silent success on nothing.
func (r *pgRepository) DeleteTaskItem(ctx context.Context, tx database.Tx, itemID, taskID, tenantID string) error {
	id, err := parseUUID(itemID)
	if err != nil {
		return err
	}
	task, err := parseUUID(taskID)
	if err != nil {
		return err
	}
	tenant, err := parseUUID(tenantID)
	if err != nil {
		return err
	}

	_, err = deadlinedb.New(tx).DeleteTaskItem(ctx, deadlinedb.DeleteTaskItemParams{
		ID:       id,
		TaskID:   task,
		TenantID: tenant,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrTaskItemNotFound
	}
	if err != nil {
		return database.WrapInfra(err)
	}
	return nil
}

// GetDeadlineForCheck re-reads the prazo by id inside the caller's tx, filtered by tenantID
// (barrier 1). A missing id — or one in another tenant — maps to the typed
// ErrDeadlineNotFound (never nil, nil); a NULL kind returns "". The mapper absorbs the
// driver types (uuid.UUID, pgtype.Date) so the use case sees a pure *DeadlineForCheck.
func (r *pgRepository) GetDeadlineForCheck(ctx context.Context, tx database.Tx, deadlineID, tenantID string) (*DeadlineForCheck, error) {
	id, err := parseUUID(deadlineID)
	if err != nil {
		return nil, err
	}
	tenant, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}

	row, err := deadlinedb.New(tx).GetDeadlineForCheck(ctx, deadlinedb.GetDeadlineForCheckParams{
		ID:       id,
		TenantID: tenant,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrDeadlineNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}

	return &DeadlineForCheck{
		ID:            row.ID.String(),
		Status:        Status(row.Status),
		EndDate:       row.EndDate.Time,
		CourtRecordID: row.CourtRecordID.String(),
		Kind:          derefString(row.Kind),
		Counting:      Counting(row.Counting),
	}, nil
}

// MarkMissed auto-flips the prazo to MISSED inside the caller's tx, filtered by tenantID
// (barrier 1). The query's status='OPEN' AND end_date < CURRENT_DATE guard means a
// redelivery — or a PENDING/terminal/not-yet-overdue prazo — updates no row: sqlc returns
// pgx.ErrNoRows, mapped to the typed ErrDeadlineNotFound so the use case no-ops instead of
// emitting a phantom missed. On a hit it returns the missed prazo's id.
func (r *pgRepository) MarkMissed(ctx context.Context, tx database.Tx, deadlineID, tenantID string) (string, error) {
	id, err := parseUUID(deadlineID)
	if err != nil {
		return "", err
	}
	tenant, err := parseUUID(tenantID)
	if err != nil {
		return "", err
	}

	missed, err := deadlinedb.New(tx).MarkMissed(ctx, deadlinedb.MarkMissedParams{
		ID:       id,
		TenantID: tenant,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrDeadlineNotFound
	}
	if err != nil {
		return "", database.WrapInfra(err)
	}
	return missed.String(), nil
}

// RevokeDeadlineByIntimation cancels the prazo derived from the intimação inside the
// caller's tx, filtered by tenantID (barrier 1). The query's status <> 'CANCELLED' guard
// means a redelivery — or a cancel that arrives before (or without) any prazo — updates no
// row: sqlc returns pgx.ErrNoRows, mapped to the typed ErrDeadlineNotFound so the use case
// no-ops instead of emitting a phantom revoked (never nil, nil). On a hit it returns the
// revoked prazo's id and the record it hung on. IntimationID is matched against the
// notification_id column (the historic-name FK to intimation — see mapper.go).
func (r *pgRepository) RevokeDeadlineByIntimation(ctx context.Context, tx database.Tx, intimationID, tenantID string) (*RevokedDeadline, error) {
	intID, err := parseUUID(intimationID)
	if err != nil {
		return nil, err
	}
	tenant, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}

	row, err := deadlinedb.New(tx).RevokeDeadlineByIntimation(ctx, deadlinedb.RevokeDeadlineByIntimationParams{
		NotificationID: intID,
		TenantID:       tenant,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrDeadlineNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}

	return &RevokedDeadline{
		ID:            row.ID.String(),
		CourtRecordID: row.CourtRecordID.String(),
	}, nil
}

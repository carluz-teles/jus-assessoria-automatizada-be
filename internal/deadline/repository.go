package deadline

import (
	"context"
	"encoding/json"
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
		RulesVersion:  row.RulesVersion,
		Kind:          row.Kind,
		Days:          int(row.Days),
		Counting:      Counting(row.Counting),
		Doubled:       row.Doubled,
		LegalCitation: derefString(row.LegalCitation),
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
		TenantID:           tenantID,
		CourtRecordID:      courtRecordID,
		NotificationID:     intimationID,
		StartDate:          pgDate(d.StartDate),
		EndDate:            pgDate(d.EndDate),
		PrazoInterno:       pgDate(d.PrazoInterno),
		Days:               int32(d.Days),
		Counting:           string(d.Counting),
		Doubled:            d.Doubled,
		DoubledReason:      textToNull(d.DoubledReason),
		HolidaysApplied:    holidays,
		Status:             string(d.Status),
		Source:             string(d.Source),
		Kind:               textToNull(d.Kind),
		RulesVersion:       d.RulesVersion,
		AnchorEvent:        string(d.AnchorEvent),
		LegalCitation:      textToNull(d.LegalCitation),
		Origem:             textToNull(string(d.Origem)),
		Selo:               textToNull(string(d.Seal)),
		ConfirmacaoExigida: &d.ConfirmacaoExigida,
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
		LegalCitation: derefString(row.LegalCitation),
	}, nil
}

// GetIntimationAnchors loads the intimação's three observed dates inside the caller's tx,
// filtered by tenantID (barrier 1). A missing/foreign intimação maps to the typed
// ErrDeadlineNotFound (the prazo's anchor cannot be resolved). All three columns are NOT NULL.
func (r *pgRepository) GetIntimationAnchors(ctx context.Context, tx database.Tx, intimationID, tenantID string) (IntimationAnchors, error) {
	intID, err := parseUUID(intimationID)
	if err != nil {
		return IntimationAnchors{}, err
	}
	tenant, err := parseUUID(tenantID)
	if err != nil {
		return IntimationAnchors{}, err
	}

	row, err := deadlinedb.New(tx).GetIntimationAnchors(ctx, deadlinedb.GetIntimationAnchorsParams{
		ID:       intID,
		TenantID: tenant,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return IntimationAnchors{}, ErrDeadlineNotFound
	}
	if err != nil {
		return IntimationAnchors{}, database.WrapInfra(err)
	}

	return IntimationAnchors{
		MadeAvailableAt: row.MadeAvailableAt.Time,
		PublishedAt:     row.PublishedAt.Time,
		DeadlineStartAt: row.DeadlineStartAt.Time,
	}, nil
}

// GetIntimationAssignee reads ONLY the intimação's assignee_user_id inside the caller's
// tx, filtered by tenantID (barrier 1). A missing/foreign intimação maps to the typed
// ErrIntimationNotFound. A NULL column (no responsável) is a valid nil, not an error.
func (r *pgRepository) GetIntimationAssignee(ctx context.Context, tx database.Tx, intimationID, tenantID string) (*string, error) {
	intID, err := parseUUID(intimationID)
	if err != nil {
		return nil, err
	}
	tenant, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}

	assignee, err := deadlinedb.New(tx).GetIntimationAssignee(ctx, deadlinedb.GetIntimationAssigneeParams{
		IntimationID: intID,
		TenantID:     tenant,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrIntimationNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	if !assignee.Valid {
		return nil, nil
	}
	id := uuidText(assignee)
	return &id, nil
}

// GetPreviewContext loads the preview's anchors + court inside the caller's tx (the pool, on the
// read-only preview path), filtered by tenantID (barrier 1). A missing/foreign intimação maps to
// the typed ErrDeadlineNotFound.
func (r *pgRepository) GetPreviewContext(ctx context.Context, tx database.Tx, intimationID, tenantID string) (PreviewContext, error) {
	intID, err := parseUUID(intimationID)
	if err != nil {
		return PreviewContext{}, err
	}
	tenant, err := parseUUID(tenantID)
	if err != nil {
		return PreviewContext{}, err
	}

	row, err := deadlinedb.New(tx).GetPreviewContext(ctx, deadlinedb.GetPreviewContextParams{
		ID:       intID,
		TenantID: tenant,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return PreviewContext{}, ErrDeadlineNotFound
	}
	if err != nil {
		return PreviewContext{}, database.WrapInfra(err)
	}

	return PreviewContext{
		Anchors: IntimationAnchors{
			MadeAvailableAt: row.MadeAvailableAt.Time,
			PublishedAt:     row.PublishedAt.Time,
			DeadlineStartAt: row.DeadlineStartAt.Time,
		},
		Court: row.Court,
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
		PrazoInterno:    pgDate(p.PrazoInterno),
		HolidaysApplied: holidays,
		ConfirmedBy:     confirmedBy,
		ConfirmedAt:     pgTimestamptz(p.ConfirmedAt),
		StartDate:       pgDate(p.StartDate),
		AnchorEvent:     string(p.AnchorEvent),
		ManualExtraDays: int32(p.ManualExtraDays),
		LegalCitation:   textToNull(p.LegalCitation),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrDeadlineNotFound
	}
	if err != nil {
		return "", "", database.WrapInfra(err)
	}
	return row.ID.String(), row.CourtRecordID.String(), nil
}

// ListTaskTitlesByDeadline reads the titles of the tasks currently associated with the
// confirmed prazo inside the caller's tx, scoped to (deadlineID, tenantID) (barrier 1 + RLS
// barrier 2). The F2 confirm reads it to diff the AI suggestion against the tasks that really
// exist (feedback loop, camada 2). deadline_id is a nullable column (mapper lifts it to
// pgtype.UUID); tenant_id is NOT NULL. A prazo with no tasks yields an empty slice (sqlc's
// :many never returns pgx.ErrNoRows), so absence is a clean empty result, not an error.
func (r *pgRepository) ListTaskTitlesByDeadline(ctx context.Context, tx database.Tx, deadlineID, tenantID string) ([]string, error) {
	id, err := pgUUID(deadlineID)
	if err != nil {
		return nil, err
	}
	tenant, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}

	titles, err := deadlinedb.New(tx).ListTaskTitlesByDeadline(ctx, deadlinedb.ListTaskTitlesByDeadlineParams{
		DeadlineID: id,
		TenantID:   tenant,
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	return titles, nil
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
	// created_by is OPTIONAL (task.created_by is nullable, 0024): every manual/F2 path fills
	// it with the principal, but fatia 3's automatic path (createTaskFromActionItem) has no
	// human creator — pgOptionalUUID ("" → NULL), not pgUUID, so that path never fails here.
	createdBy, err := pgOptionalUUID(t.CreatedBy)
	if err != nil {
		return nil, err
	}

	actionItemID, err := pgOptionalUUID(t.ActionItemID)
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
		Priority:       textToNull(t.Priority),
		DueDate:        pgOptionalDate(t.DueDate),
		Status:         string(t.Status),
		Source:         string(t.Source),
		AssigneeUserID: assignee,
		CreatedBy:      createdBy,
		ActionItemID:   actionItemID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// ON CONFLICT (action_item_id) DO NOTHING yielded no row: a task already exists for
		// this providência (0087's UNIQUE). Only reachable when t.ActionItemID is set — the
		// manual/avulsa path's action_item_id is always NULL, which never conflicts.
		return nil, ErrTaskExistsForActionItem
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}

	saved := *t
	saved.ID = id.String()
	return &saved, nil
}

// GetActionItemCourtRecordID reads action_item.court_record_id inside the caller's tx,
// filtered by tenantID (barrier 1) — fatia 3's decisão P1 read (see the Repository doc). A
// missing/foreign action_item id maps to the typed ErrActionItemNotFound (never "", nil); a
// present row with a NULL court_record_id returns "".
func (r *pgRepository) GetActionItemCourtRecordID(ctx context.Context, tx database.Tx, tenantID, actionItemID string) (string, error) {
	id, err := parseUUID(actionItemID)
	if err != nil {
		return "", err
	}
	tenant, err := parseUUID(tenantID)
	if err != nil {
		return "", err
	}

	courtRecordID, err := deadlinedb.New(tx).GetActionItemCourtRecordID(ctx, deadlinedb.GetActionItemCourtRecordIDParams{
		ID:       id,
		TenantID: tenant,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrActionItemNotFound
	}
	if err != nil {
		return "", database.WrapInfra(err)
	}
	return uuidText(courtRecordID), nil
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
		Priority:       derefString(row.Priority),
		DueDate:        datePtr(row.DueDate),
		AssigneeUserID: uuidText(row.AssigneeUserID),
		DeadlineID:     uuidText(row.DeadlineID),
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
		Priority:       textToNull(p.Priority),
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
		Priority:       derefString(row.Priority),
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

// EnsureTaskExistsInTenant confirms a task exists in the tenant inside the caller's tx — the
// guard the comment create runs first. Unlike EnsureTaskInTenant (checklist items, which reports
// ErrTaskItemNotFound), a miss here is ErrTaskNotFound (→ 404): a comment on a foreign/unknown
// task is a task miss, not an item miss. Reuses the same TaskExistsInTenant query.
func (r *pgRepository) EnsureTaskExistsInTenant(ctx context.Context, tx database.Tx, taskID, tenantID string) error {
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
		return ErrTaskNotFound
	}
	if err != nil {
		return database.WrapInfra(err)
	}
	return nil
}

// InsertTaskComment appends one comment to a task's thread inside the caller's tx and returns it
// with its DB-assigned id + created_at (echoing the entity, like InsertTaskItem). tenant_id/
// task_id/author_user_id are NOT NULL uuids; the mapper lifts them via pgUUID (a malformed value
// is an infra fault). The parent-task guard runs in the use case before this write.
func (r *pgRepository) InsertTaskComment(ctx context.Context, tx database.Tx, c *TaskComment) (*TaskComment, error) {
	tenant, err := parseUUID(c.TenantID)
	if err != nil {
		return nil, err
	}
	taskID, err := parseUUID(c.TaskID)
	if err != nil {
		return nil, err
	}
	author, err := parseUUID(c.AuthorUserID)
	if err != nil {
		return nil, err
	}

	row, err := deadlinedb.New(tx).InsertTaskComment(ctx, deadlinedb.InsertTaskCommentParams{
		TenantID:     tenant,
		TaskID:       taskID,
		AuthorUserID: author,
		Body:         c.Body,
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}

	saved := *c
	saved.ID = row.ID.String()
	saved.CreatedAt = row.CreatedAt.Time
	return &saved, nil
}

// InsertTaskActivity appends one audit-log row inside the caller's tx (called on the SAME tx as
// the mutation it records). tenant_id/task_id/actor_user_id are NOT NULL uuids; event_type is the
// closed set (validated by the caller). from/to are nullable text ("" → NULL via textToNull). The
// caller does not need the row back (the Atividade tab re-reads the whole log), so it returns only
// the error.
func (r *pgRepository) InsertTaskActivity(ctx context.Context, tx database.Tx, a *TaskActivity) error {
	tenant, err := parseUUID(a.TenantID)
	if err != nil {
		return err
	}
	taskID, err := parseUUID(a.TaskID)
	if err != nil {
		return err
	}
	actor, err := parseUUID(a.ActorUserID)
	if err != nil {
		return err
	}

	_, err = deadlinedb.New(tx).InsertTaskActivity(ctx, deadlinedb.InsertTaskActivityParams{
		TenantID:    tenant,
		TaskID:      taskID,
		ActorUserID: actor,
		EventType:   string(a.EventType),
		FromValue:   textToNull(a.FromValue),
		ToValue:     textToNull(a.ToValue),
	})
	if err != nil {
		return database.WrapInfra(err)
	}
	return nil
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
		ID:              row.ID.String(),
		CourtRecordID:   row.CourtRecordID.String(),
		IntimationID:    row.NotificationID.String(),
		StartDate:       row.StartDate.Time,
		Status:          Status(row.Status),
		Kind:            derefString(row.Kind),
		Days:            int(row.Days),
		Counting:        Counting(row.Counting),
		Doubled:         row.Doubled,
		DoubledReason:   derefString(row.DoubledReason),
		AnchorEvent:     AnchorEvent(row.AnchorEvent),
		ManualExtraDays: int(row.ManualExtraDays),
		Origem:          Origem(derefString(row.Origem)),
		Selo:            Seal(derefString(row.Selo)),
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
		PrazoInterno:    pgDate(p.PrazoInterno),
		HolidaysApplied: holidays,
		StartDate:       pgDate(p.StartDate),
		AnchorEvent:     string(p.AnchorEvent),
		ManualExtraDays: int32(p.ManualExtraDays),
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

// MarkNoDeadline flips a prazo PENDING|OPEN → NO_DEADLINE inside the caller's tx, stamping
// confirmed_by/at, keyed by its id and filtered by tenantID (barrier 1). The guarded UPDATE
// (status IN ('PENDING','OPEN')) collapses any other case to a no-op → pgx.ErrNoRows →
// ErrDeadlineNotFound. On a hit it returns the flipped id.
func (r *pgRepository) MarkNoDeadline(ctx context.Context, tx database.Tx, deadlineID, tenantID, confirmedBy string, confirmedAt time.Time) (string, error) {
	id, err := parseUUID(deadlineID)
	if err != nil {
		return "", err
	}
	tenant, err := parseUUID(tenantID)
	if err != nil {
		return "", err
	}
	by, err := pgUUID(confirmedBy)
	if err != nil {
		return "", err
	}

	flipped, err := deadlinedb.New(tx).MarkNoDeadline(ctx, deadlinedb.MarkNoDeadlineParams{
		ID:          id,
		TenantID:    tenant,
		ConfirmedBy: by,
		ConfirmedAt: pgTimestamptz(confirmedAt),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrDeadlineNotFound
	}
	if err != nil {
		return "", database.WrapInfra(err)
	}
	return flipped.String(), nil
}

// ReopenNoDeadline reverts a NO_DEADLINE prazo → PENDING inside the caller's tx, clearing
// confirmed_by/at, keyed by its id and filtered by tenantID (barrier 1). The guarded UPDATE
// (status = 'NO_DEADLINE') collapses any other case to a no-op → pgx.ErrNoRows →
// ErrDeadlineNotFound. On a hit it returns the reopened id.
func (r *pgRepository) ReopenNoDeadline(ctx context.Context, tx database.Tx, deadlineID, tenantID string) (string, error) {
	id, err := parseUUID(deadlineID)
	if err != nil {
		return "", err
	}
	tenant, err := parseUUID(tenantID)
	if err != nil {
		return "", err
	}

	flipped, err := deadlinedb.New(tx).ReopenNoDeadline(ctx, deadlinedb.ReopenNoDeadlineParams{
		ID:       id,
		TenantID: tenant,
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

// GetDeadlineEndDate reads ONLY a prazo's end_date inside the caller's tx, filtered by tenantID
// (barrier 1). The task write path (POST/PATCH /v1/tasks) uses it to enforce ERD §4's task
// invariant (a task's due_date cannot fall after its prazo's end_date). A missing id — or one in
// another tenant — maps to the typed ErrDeadlineNotFound (never zero, nil); the mapper absorbs
// the pgtype.Date so the use case sees a pure time.Time.
func (r *pgRepository) GetDeadlineEndDate(ctx context.Context, tx database.Tx, deadlineID, tenantID string) (time.Time, error) {
	id, err := parseUUID(deadlineID)
	if err != nil {
		return time.Time{}, err
	}
	tenant, err := parseUUID(tenantID)
	if err != nil {
		return time.Time{}, err
	}

	endDate, err := deadlinedb.New(tx).GetDeadlineEndDate(ctx, deadlinedb.GetDeadlineEndDateParams{
		ID:       id,
		TenantID: tenant,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, ErrDeadlineNotFound
	}
	if err != nil {
		return time.Time{}, database.WrapInfra(err)
	}
	return endDate.Time, nil
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

// ListReconcilableDeadlines loads the MISSED/OPEN prazos of a court_record inside the caller's
// tx, filtered by tenantID (barrier 1). It reads the deadline table directly for the docket-
// entry reconcile. No reconcilable prazo yields an empty slice (a :many never returns
// pgx.ErrNoRows), never an error; the mapper absorbs the driver types (uuid.UUID, pgtype.Date).
func (r *pgRepository) ListReconcilableDeadlines(ctx context.Context, tx database.Tx, courtRecordID, tenantID string) ([]ReconcilableDeadline, error) {
	recordID, err := parseUUID(courtRecordID)
	if err != nil {
		return nil, err
	}
	tenant, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}

	rows, err := deadlinedb.New(tx).ListReconcilableDeadlines(ctx, deadlinedb.ListReconcilableDeadlinesParams{
		CourtRecordID: recordID,
		TenantID:      tenant,
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}

	out := make([]ReconcilableDeadline, len(rows))
	for i, row := range rows {
		out[i] = ReconcilableDeadline{
			ID:        row.ID.String(),
			StartDate: row.StartDate.Time,
		}
	}
	return out, nil
}

// HasResponseMovement reports whether the court_record holds an andamento de resposta on/after
// startDate inside the caller's tx, filtered by tenantID (barrier 1) via the JOIN — the ONE
// cross-table read the reconcile needs (decisão P1: read docket_entry directly, never import
// acquisition). start_date (a civil date) is lifted to a timestamptz so it compares against the
// occurred_at column. It returns a plain bool (an EXISTS), so absence is false, not an error.
func (r *pgRepository) HasResponseMovement(ctx context.Context, tx database.Tx, courtRecordID, tenantID string, startDate time.Time, tpuCodes []int32) (bool, error) {
	recordID, err := parseUUID(courtRecordID)
	if err != nil {
		return false, err
	}
	tenant, err := parseUUID(tenantID)
	if err != nil {
		return false, err
	}

	has, err := deadlinedb.New(tx).HasResponseMovement(ctx, deadlinedb.HasResponseMovementParams{
		ID:         recordID,
		TenantID:   tenant,
		OccurredAt: pgTimestamptz(startDate),
		TpuCodes:   tpuCodes,
	})
	if err != nil {
		return false, database.WrapInfra(err)
	}
	return has, nil
}

// MarkMet reconciles the prazo MISSED/OPEN → MET inside the caller's tx, filtered by tenantID
// (barrier 1). The query's status IN ('MISSED','OPEN') guard means a redelivery — or an
// already-MET/CANCELLED/PENDING prazo — updates no row: sqlc returns pgx.ErrNoRows, mapped to
// the typed ErrDeadlineNotFound so the use case no-ops instead of emitting a phantom met. On a
// hit it returns the reconciled prazo's id. It mirrors MarkMissed's shape.
func (r *pgRepository) MarkMet(ctx context.Context, tx database.Tx, deadlineID, tenantID string) (string, error) {
	id, err := parseUUID(deadlineID)
	if err != nil {
		return "", err
	}
	tenant, err := parseUUID(tenantID)
	if err != nil {
		return "", err
	}

	met, err := deadlinedb.New(tx).MarkMet(ctx, deadlinedb.MarkMetParams{
		ID:       id,
		TenantID: tenant,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrDeadlineNotFound
	}
	if err != nil {
		return "", database.WrapInfra(err)
	}
	return met.String(), nil
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

// GetLatestSuggestion reads the most recent AI suggestion for a prazo (by the 1:1
// intimação, scoped to tenantID — barrier 1) inside the caller's tx, for the F2 confirm's
// delta. A MISS is not an error: no suggestion means the lawyer never asked the IA (or the
// prazo predates the suggester), so it returns (zero, false, nil) and the confirm emits no
// feedback. The suggested jsonb is the exact [{title, kind}, …] the model returned; a
// malformed payload (should never happen — the write goes through the same slice) is a
// typed infra error, not a panic.
func (r *pgRepository) GetLatestSuggestion(ctx context.Context, tx database.Tx, tenantID, intimationID string) (SuggestionRecord, bool, error) {
	intID, err := parseUUID(intimationID)
	if err != nil {
		return SuggestionRecord{}, false, err
	}
	tenant, err := parseUUID(tenantID)
	if err != nil {
		return SuggestionRecord{}, false, err
	}

	row, err := deadlinedb.New(tx).GetLatestTaskSuggestion(ctx, deadlinedb.GetLatestTaskSuggestionParams{
		IntimationID: intID,
		TenantID:     tenant,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return SuggestionRecord{}, false, nil
	}
	if err != nil {
		return SuggestionRecord{}, false, database.WrapInfra(err)
	}

	var tasks []SuggestedTask
	if err := json.Unmarshal(row.Suggested, &tasks); err != nil {
		return SuggestionRecord{}, false, database.WrapInfra(err)
	}

	return SuggestionRecord{
		IntimationID:  intimationID,
		PromptVersion: row.PromptVersion,
		Model:         row.Model,
		Tasks:         tasks,
	}, true, nil
}

// InsertCalcMemory persists the deterministic calculation audit trail inside the
// caller's tx, 1:1 on deadline_id. The ON CONFLICT DO NOTHING on the UNIQUE
// deadline_id means a re-derivation yields NO row → pgx.ErrNoRows → typed
// ErrCalcMemoryExists at the mapper, so the use case no-ops instead of poisoning the
// tx with a constraint error. The mapper absorbs the entity's value types into the
// generated param struct; nullable bool (*bool) maps directly from the entity.
func (r *pgRepository) InsertCalcMemory(ctx context.Context, tx database.Tx, m *CalcMemory) (*CalcMemory, error) {
	tenantID, err := parseUUID(m.TenantID)
	if err != nil {
		return nil, err
	}
	deadlineID, err := parseUUID(m.DeadlineID)
	if err != nil {
		return nil, err
	}

	id, err := deadlinedb.New(tx).InsertCalcMemory(ctx, deadlinedb.InsertCalcMemoryParams{
		TenantID:                tenantID,
		DeadlineID:              deadlineID,
		PrazoBase:               textToNull(m.PrazoBase),
		PrazoBaseFonte:          textToNull(m.PrazoBaseFonte),
		TermoInicialRegra:       textToNull(m.TermoInicialRegra),
		DiasUteis:               optionalBool(m.DiasUteis),
		DobraMotivo:             textToNull(m.DobraMotivo),
		TabelaLegalRef:          textToNull(m.TabelaLegalRef),
		IaTipoInferido:          textToNull(m.IATipoInferido),
		IaConfianca:             &m.IAConfianca,
		CalendarProviderVersion: textToNull(m.CalendarProviderVersion),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrCalcMemoryExists
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}

	saved := *m
	saved.ID = id.String()
	return &saved, nil
}

// InsertAppliedHoliday persists one holiday snapshot inside the caller's tx, 1:N
// under the parent calc_memory. The mapper absorbs the entity's value types;
// nullable text columns map via textToNull; the date maps via pgDate.
func (r *pgRepository) InsertAppliedHoliday(ctx context.Context, tx database.Tx, h *AppliedHoliday) (*AppliedHoliday, error) {
	tenantID, err := parseUUID(h.TenantID)
	if err != nil {
		return nil, err
	}
	calcMemoryID, err := parseUUID(h.CalcMemoryID)
	if err != nil {
		return nil, err
	}

	id, err := deadlinedb.New(tx).InsertAppliedHoliday(ctx, deadlinedb.InsertAppliedHolidayParams{
		TenantID:     tenantID,
		CalcMemoryID: calcMemoryID,
		Data:         pgDate(h.Data),
		Nome:         textToNull(h.Nome),
		Ambito:       textToNull(h.Ambito),
		Comarca:      textToNull(h.Comarca),
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}

	saved := *h
	saved.ID = id.String()
	return &saved, nil
}

// InsertCrossValidation persists the declared vs calculated cross-validation result
// inside the caller's tx, 1:1 on deadline_id. The ON CONFLICT DO NOTHING on the
// UNIQUE deadline_id means a re-derivation yields NO row → pgx.ErrNoRows → typed
// ErrCrossValidationExists at the mapper, so the use case no-ops. decidido_por is
// nullable (NULL when the system decided), mapped via pgOptionalUUID.
func (r *pgRepository) InsertCrossValidation(ctx context.Context, tx database.Tx, cv *CrossValidation) (*CrossValidation, error) {
	tenantID, err := parseUUID(cv.TenantID)
	if err != nil {
		return nil, err
	}
	deadlineID, err := parseUUID(cv.DeadlineID)
	if err != nil {
		return nil, err
	}
	decididoPor, err := pgOptionalUUID(cv.DecididoPor)
	if err != nil {
		return nil, err
	}

	id, err := deadlinedb.New(tx).InsertCrossValidation(ctx, deadlinedb.InsertCrossValidationParams{
		TenantID:      tenantID,
		DeadlineID:    deadlineID,
		DataDeclarada: pgDate(cv.DataDeclarada),
		DataCalculada: pgDate(cv.DataCalculada),
		DifDias:       optionalInt32(cv.DifDias),
		Resultado:     textToNull(cv.Resultado),
		CausaProvavel: textToNull(cv.CausaProvavel),
		Decisao:       textToNull(cv.Decisao),
		DecididoPor:   decididoPor,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrCrossValidationExists
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}

	saved := *cv
	saved.ID = id.String()
	return &saved, nil
}

// InsertDeadlineEvent appends one event to the deadline's append-only audit trail
// inside the caller's tx. deadline_event never overwrites; it adds — history is
// auditable. ator_id is nullable (NULL when the system is the actor), mapped via
// pgOptionalUUID. The caller does not need the row back (the audit trail re-reads
// the whole log), so it returns only the error.
func (r *pgRepository) InsertDeadlineEvent(ctx context.Context, tx database.Tx, e *DeadlineEvent) error {
	tenantID, err := parseUUID(e.TenantID)
	if err != nil {
		return err
	}
	deadlineID, err := parseUUID(e.DeadlineID)
	if err != nil {
		return err
	}
	atorID, err := pgOptionalUUID(e.AtorID)
	if err != nil {
		return err
	}

	_, err = deadlinedb.New(tx).InsertDeadlineEvent(ctx, deadlinedb.InsertDeadlineEventParams{
		TenantID:   tenantID,
		DeadlineID: deadlineID,
		Tipo:       e.Tipo,
		Detalhe:    textToNull(e.Detalhe),
		AtorID:     atorID,
	})
	if err != nil {
		return database.WrapInfra(err)
	}
	return nil
}

// GetPolicy reads the tenant's deadline confirmation policy inside the caller's tx.
// A row that exists returns the stored policy; the absence of a row (no explicit
// opt-in) is NOT an error — it returns the seletiva default (ConfirmacaoObrigatoria
// = false), because the absence means the tenant never chose strict mode. This maps
// pgx.ErrNoRows to a default value rather than a typed not-found, which is a deliberate
// divergence from the usual "absence = error" pattern: the policy is configuration, not
// a domain entity, and every tenant always has a policy (the default is the policy).
func (r *pgRepository) GetPolicy(ctx context.Context, tx database.Tx, tenantID string) (DeadlinePolicy, error) {
	tenant, err := parseUUID(tenantID)
	if err != nil {
		return DeadlinePolicy{}, err
	}

	row, err := deadlinedb.New(tx).GetDeadlinePolicy(ctx, tenant)
	if errors.Is(err, pgx.ErrNoRows) {
		return DeadlinePolicy{
			TenantID:               tenantID,
			ConfirmacaoObrigatoria: false,
		}, nil
	}
	if err != nil {
		return DeadlinePolicy{}, database.WrapInfra(err)
	}

	return DeadlinePolicy{
		TenantID:               row.TenantID.String(),
		ConfirmacaoObrigatoria: row.ConfirmacaoObrigatoria,
	}, nil
}

// GetCrossValidation reads the declared×calculado cross-validation for one deadline inside the
// caller's tx, scoped to tenantID (barrier 1). A missing row (no prazo_declarado at birth, so
// nothing was ever cross-checked) maps to the typed ErrCrossValidationNotFound (never nil, nil).
func (r *pgRepository) GetCrossValidation(ctx context.Context, tx database.Tx, tenantID, deadlineID string) (*CrossValidation, error) {
	deadline, err := parseUUID(deadlineID)
	if err != nil {
		return nil, err
	}
	tenant, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}

	row, err := deadlinedb.New(tx).GetCrossValidation(ctx, deadlinedb.GetCrossValidationParams{
		DeadlineID: deadline,
		TenantID:   tenant,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrCrossValidationNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}

	return &CrossValidation{
		ID:            row.ID.String(),
		TenantID:      tenantID,
		DeadlineID:    deadlineID,
		DataDeclarada: row.DataDeclarada.Time,
		DataCalculada: row.DataCalculada.Time,
		DifDias:       derefInt32(row.DifDias),
		Resultado:     derefString(row.Resultado),
		CausaProvavel: derefString(row.CausaProvavel),
		Decisao:       derefString(row.Decisao),
		DecididoPor:   uuidText(row.DecididoPor),
	}, nil
}

// UpdateCrossValidationDecision records the human's divergência decision inside the caller's tx,
// keyed by deadline_id and scoped to tenantID (barrier 1). An UPDATE (the row already exists
// since InsertCrossValidation ran at birth). The query's `decisao IS NULL` guard is the
// concurrency floor (mirroring MarkTaskStatus/MarkDeadlineStatus's `status = current_status`
// guard): the use case pre-checks decisao=="" before calling, and this guard defends the write
// against a racing second apuração on the SAME divergência. By the time this runs,
// GetCrossValidation has already confirmed the row exists, so a no-match here can only mean a
// concurrent apuração already decided it — mapped to the typed ErrDeadlineNotDivergent (never a
// silent overwrite of the first decision).
func (r *pgRepository) UpdateCrossValidationDecision(ctx context.Context, tx database.Tx, tenantID, deadlineID, decisao, decididoPor string) error {
	deadline, err := parseUUID(deadlineID)
	if err != nil {
		return err
	}
	tenant, err := parseUUID(tenantID)
	if err != nil {
		return err
	}
	decidedBy, err := pgOptionalUUID(decididoPor)
	if err != nil {
		return err
	}

	_, err = deadlinedb.New(tx).UpdateCrossValidationDecision(ctx, deadlinedb.UpdateCrossValidationDecisionParams{
		DeadlineID:  deadline,
		TenantID:    tenant,
		Decisao:     textToNull(decisao),
		DecididoPor: decidedBy,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrDeadlineNotDivergent
	}
	if err != nil {
		return database.WrapInfra(err)
	}
	return nil
}

// UpdateDeadlineEndDate overwrites end_date AND prazo_interno inside the caller's tx, keyed by
// the prazo id and scoped to tenantID (barrier 1) — the "aceita_declarado" apuração write (no
// recompute of days/counting/holidays_applied; prazo_interno is the caller's recompute from the
// new end_date, so it never drifts). A no-match maps to the typed ErrDeadlineNotFound.
func (r *pgRepository) UpdateDeadlineEndDate(ctx context.Context, tx database.Tx, tenantID, deadlineID string, endDate, prazoInterno time.Time) error {
	id, err := parseUUID(deadlineID)
	if err != nil {
		return err
	}
	tenant, err := parseUUID(tenantID)
	if err != nil {
		return err
	}

	_, err = deadlinedb.New(tx).UpdateDeadlineEndDate(ctx, deadlinedb.UpdateDeadlineEndDateParams{
		ID:           id,
		TenantID:     tenant,
		EndDate:      pgDate(endDate),
		PrazoInterno: pgDate(prazoInterno),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrDeadlineNotFound
	}
	if err != nil {
		return database.WrapInfra(err)
	}
	return nil
}

// UpdateDeadlineSelo flips the confidence selo AND stamps confirmed_by/confirmed_at inside the
// caller's tx, keyed by the prazo id and scoped to tenantID (barrier 1) — origem is NEVER written
// here (immutable after creation). The query's `selo = 'a_apurar'` guard is the concurrency floor
// (mirroring MarkTaskStatus/MarkDeadlineStatus's `status = current_status` guard): the use case
// pre-checks the selo before calling, and this guard defends the write against a racing second
// apuração on the SAME prazo. By the time this runs, GetDeadlineForAdjust has already confirmed
// the row exists, so a no-match here can only mean a concurrent apuração already sealed it
// confiavel — mapped to the typed ErrDeadlineNotDivergent (never a silent no-op re-seal).
func (r *pgRepository) UpdateDeadlineSelo(ctx context.Context, tx database.Tx, tenantID, deadlineID string, selo Seal, confirmedBy string, confirmedAt time.Time) error {
	id, err := parseUUID(deadlineID)
	if err != nil {
		return err
	}
	tenant, err := parseUUID(tenantID)
	if err != nil {
		return err
	}
	by, err := pgUUID(confirmedBy)
	if err != nil {
		return err
	}

	_, err = deadlinedb.New(tx).UpdateDeadlineSelo(ctx, deadlinedb.UpdateDeadlineSeloParams{
		ID:          id,
		TenantID:    tenant,
		Selo:        textToNull(string(selo)),
		ConfirmedBy: by,
		ConfirmedAt: pgTimestamptz(confirmedAt),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrDeadlineNotDivergent
	}
	if err != nil {
		return database.WrapInfra(err)
	}
	return nil
}

// GetCalcMemory reads one deadline's calc_memory inside the caller's tx, scoped to tenantID
// (barrier 1). A missing row maps to the typed ErrCalcMemoryNotFound (never nil, nil).
func (r *pgRepository) GetCalcMemory(ctx context.Context, tx database.Tx, tenantID, deadlineID string) (*CalcMemory, error) {
	deadline, err := parseUUID(deadlineID)
	if err != nil {
		return nil, err
	}
	tenant, err := parseUUID(tenantID)
	if err != nil {
		return nil, err
	}

	row, err := deadlinedb.New(tx).GetCalcMemory(ctx, deadlinedb.GetCalcMemoryParams{
		DeadlineID: deadline,
		TenantID:   tenant,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrCalcMemoryNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}

	return &CalcMemory{
		ID:                      row.ID.String(),
		TenantID:                tenantID,
		DeadlineID:              deadlineID,
		PrazoBase:               derefString(row.PrazoBase),
		PrazoBaseFonte:          derefString(row.PrazoBaseFonte),
		TermoInicialRegra:       derefString(row.TermoInicialRegra),
		DiasUteis:               derefBool(row.DiasUteis),
		DobraMotivo:             derefString(row.DobraMotivo),
		TabelaLegalRef:          derefString(row.TabelaLegalRef),
		IATipoInferido:          derefString(row.IaTipoInferido),
		IAConfianca:             derefFloat64(row.IaConfianca),
		CalendarProviderVersion: derefString(row.CalendarProviderVersion),
	}, nil
}

// UpdateCalcMemoryTipoConfirmation records the human's confirmed/reclassified tipo de ato inside
// the caller's tx, keyed by deadline_id and scoped to tenantID (barrier 1). An UPDATE (the row
// already exists since InsertCalcMemory ran at birth). A no-match maps to the typed
// ErrCalcMemoryNotFound.
func (r *pgRepository) UpdateCalcMemoryTipoConfirmation(ctx context.Context, tx database.Tx, tenantID, deadlineID, tipo string, confianca float64) error {
	deadline, err := parseUUID(deadlineID)
	if err != nil {
		return err
	}
	tenant, err := parseUUID(tenantID)
	if err != nil {
		return err
	}

	_, err = deadlinedb.New(tx).UpdateCalcMemoryTipoConfirmation(ctx, deadlinedb.UpdateCalcMemoryTipoConfirmationParams{
		DeadlineID:     deadline,
		TenantID:       tenant,
		IaTipoInferido: textToNull(tipo),
		IaConfianca:    &confianca,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrCalcMemoryNotFound
	}
	if err != nil {
		return database.WrapInfra(err)
	}
	return nil
}

// optionalInt32 lifts a Go int to a *int32 for a nullable integer column.
// cross_validation.dif_dias is nullable (NULL when there is no declared date to
// compare against), but the mapper provides the value explicitly.
func optionalInt32(v int) *int32 {
	n := int32(v)
	return &n
}

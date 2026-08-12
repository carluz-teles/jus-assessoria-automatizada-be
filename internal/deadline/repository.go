package deadline

import (
	"context"
	"errors"

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

// InsertTask persists one F2 task inside the caller's tx and returns it with its
// DB-assigned id (echoing the entity, like InsertDeadline). tenant_id is NOT NULL; the
// context FKs (court_record_id/deadline_id/intimation_id/created_by) are always filled by
// confirm but map to nullable columns, so the mapper lifts them to pgtype.UUID;
// assignee_user_id/due_date/description/kind are optional (NULL when absent).
func (r *pgRepository) InsertTask(ctx context.Context, tx database.Tx, t *Task) (*Task, error) {
	tenant, err := parseUUID(t.TenantID)
	if err != nil {
		return nil, err
	}
	courtRecordID, err := pgUUID(t.CourtRecordID)
	if err != nil {
		return nil, err
	}
	deadlineID, err := pgUUID(t.DeadlineID)
	if err != nil {
		return nil, err
	}
	intimationID, err := pgUUID(t.IntimationID)
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

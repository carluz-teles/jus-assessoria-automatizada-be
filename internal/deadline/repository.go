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

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

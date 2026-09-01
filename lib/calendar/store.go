package calendar

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jusassessoria/platform/lib/calendar/calendardb"
)

// Store is the Postgres-backed HolidaySource and the seeder's persistence,
// wrapping the sqlc-generated calendardb.Queries. Holiday rows are shared
// reference data (no tenant_id), so the queries carry no tenant filter. This
// mapper is the one place calendardb's pgtype.* meets the lib's plain types.
type Store struct {
	q *calendardb.Queries
}

// NewStore builds a Store over the shared pool.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{q: calendardb.New(pool)}
}

var (
	_ HolidaySource = (*Store)(nil)
	_ seedStore     = (*Store)(nil)
)

// IsHoliday reports whether day is a registered holiday for the national
// calendar, the given uf, or the given court. Empty uf/court are sent as NULL,
// which never match their scope (STATE/COURT rows always carry a non-empty
// scope_id).
func (s *Store) IsHoliday(ctx context.Context, day time.Time, uf, court string) (bool, error) {
	return s.q.IsHoliday(ctx, calendardb.IsHolidayParams{
		Date:      pgDate(day),
		ScopeID:   ptrOrNil(uf),
		ScopeID_2: ptrOrNil(court),
	})
}

// LookupHolidays resolves the registered holiday row for each of the given dates, scoped to
// national/uf/court — the applied_holiday audit-trail name lookup (IsHoliday only answers a
// bool). When more than one row matches the same date (e.g. a NATIONAL holiday that also
// happens to be a COURT closure), the most specific scope wins: COURT > STATE > NATIONAL. A
// date with no matching row is simply absent from the returned map.
func (s *Store) LookupHolidays(ctx context.Context, days []time.Time, uf, court string) (map[time.Time]Holiday, error) {
	dates := make([]pgtype.Date, len(days))
	for i, day := range days {
		dates[i] = pgDate(day)
	}
	rows, err := s.q.LookupHolidays(ctx, calendardb.LookupHolidaysParams{
		Dates:        dates,
		UfScopeID:    ptrOrNil(uf),
		CourtScopeID: ptrOrNil(court),
	})
	if err != nil {
		return nil, err
	}

	result := make(map[time.Time]Holiday, len(rows))
	for _, row := range rows {
		day := dateOnly(row.Date.Time)
		scopeID := ""
		if row.ScopeID != nil {
			scopeID = *row.ScopeID
		}
		h := Holiday{Scope: row.Scope, ScopeID: scopeID, Date: day, Name: row.Name}
		if existing, ok := result[day]; !ok || scopeRank(h.Scope) > scopeRank(existing.Scope) {
			result[day] = h
		}
	}
	return result, nil
}

// scopeRank orders holiday scopes by specificity for LookupHolidays' precedence:
// COURT (most specific) > STATE > NATIONAL.
func scopeRank(scope string) int {
	switch scope {
	case ScopeCourt:
		return 2
	case ScopeState:
		return 1
	default:
		return 0
	}
}

// SeededNationalYears returns the set of years that already hold at least one
// NATIONAL holiday, so SeedNational fetches only the missing years.
func (s *Store) SeededNationalYears(ctx context.Context) (map[int]bool, error) {
	years, err := s.q.SeededNationalYears(ctx)
	if err != nil {
		return nil, err
	}
	set := make(map[int]bool, len(years))
	for _, y := range years {
		set[int(y)] = true
	}
	return set, nil
}

// UpsertHolidays inserts the given holidays idempotently (duplicates ignored via
// the unique index) and returns how many were processed. NATIONAL rows store a
// NULL scope_id. The generated batch does not surface per-row RowsAffected, so
// the count is rows submitted, not the insert/skip split.
func (s *Store) UpsertHolidays(ctx context.Context, hs []Holiday) (int, error) {
	if len(hs) == 0 {
		return 0, nil
	}
	params := make([]calendardb.UpsertHolidayParams, len(hs))
	for i, h := range hs {
		params[i] = calendardb.UpsertHolidayParams{
			Scope:   h.Scope,
			ScopeID: ptrOrNil(h.ScopeID),
			Date:    pgDate(h.Date),
			Name:    h.Name,
		}
	}

	var firstErr error
	s.q.UpsertHoliday(ctx, params).Exec(func(_ int, err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	})
	if firstErr != nil {
		return 0, fmt.Errorf("upsert holiday batch: %w", firstErr)
	}
	return len(hs), nil
}

// pgDate wraps a civil date for the `date` column (time-of-day ignored).
func pgDate(t time.Time) pgtype.Date {
	return pgtype.Date{Time: t, Valid: true}
}

// ptrOrNil maps "" to a nil *string (SQL NULL) so NATIONAL rows store scope_id
// NULL and empty uf/court never match a STATE/COURT row.
func ptrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

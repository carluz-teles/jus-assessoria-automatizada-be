package calendar

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// Scope constants for a holiday row. NATIONAL applies to the whole country
// (scope_id null), STATE to a UF (scope_id = UF), COURT to a tribunal
// (scope_id = its CNJ sigla). The forensic recess is NOT a scope — it is a fixed
// rule in inRecess, never a row.
const (
	ScopeNational = "NATIONAL"
	ScopeState    = "STATE"
	ScopeCourt    = "COURT"
)

// Holiday is one non-working day to persist. ScopeID is empty for NATIONAL, the
// UF for STATE, the tribunal sigla for COURT. Date is a civil date (time-of-day
// ignored by the `date` column).
type Holiday struct {
	Scope   string
	ScopeID string
	Date    time.Time
	Name    string
}

// Fetcher retrieves the national holidays for a given year from an upstream
// source (BrasilAPI in production). It is the seam that lets SeedNational be
// unit-tested without network I/O.
type Fetcher interface {
	NationalHolidays(ctx context.Context, year int) ([]Holiday, error)
}

// seedStore is the narrow persistence SeedNational needs — satisfied by *Store,
// faked in tests. Kept minimal so the seeding orchestration is verified without
// a database.
type seedStore interface {
	SeededNationalYears(ctx context.Context) (map[int]bool, error)
	UpsertHolidays(ctx context.Context, hs []Holiday) (int, error)
}

// SeedNational ensures the holiday table holds NATIONAL rows for every year in
// [currentYear, currentYear+yearsAhead]. It skips years already present, fetches
// the missing ones via the Fetcher and upserts them idempotently.
//
// It is deliberately fail-soft so a BrasilAPI outage never blocks boot: a year
// it cannot fetch is logged at Warn and skipped (the api must not fail because
// it could not pre-load a future year). The one dangerous case — the CURRENT
// year ending up with no national holidays, which would silently yield wrong
// deadlines — is logged at Error (alarmable) but still does not fail boot; it
// self-heals on the next boot once BrasilAPI recovers. A genuine persistence
// failure (reading seeded years, upserting) IS returned, since that signals a
// broken database, not a flaky upstream.
func SeedNational(ctx context.Context, store seedStore, fetcher Fetcher, logger *slog.Logger, currentYear, yearsAhead int) error {
	if yearsAhead < 0 {
		return fmt.Errorf("SeedNational: yearsAhead must be >= 0, got %d", yearsAhead)
	}

	seeded, err := store.SeededNationalYears(ctx)
	if err != nil {
		return fmt.Errorf("read seeded national years: %w", err)
	}

	var toUpsert []Holiday
	for year := currentYear; year <= currentYear+yearsAhead; year++ {
		if seeded[year] {
			continue
		}
		hs, err := fetcher.NationalHolidays(ctx, year)
		if err != nil {
			logger.WarnContext(ctx, "national holiday fetch failed; skipping year",
				"year", year, "error", err)
			continue
		}
		toUpsert = append(toUpsert, hs...)
	}

	if len(toUpsert) > 0 {
		count, err := store.UpsertHolidays(ctx, toUpsert)
		if err != nil {
			return fmt.Errorf("upsert national holidays: %w", err)
		}
		logger.InfoContext(ctx, "national holidays seeded", "count", count)
	}

	if !seeded[currentYear] && !hasYear(toUpsert, currentYear) {
		logger.ErrorContext(ctx, "no national holidays for the current year — deadlines may be miscomputed until BrasilAPI recovers",
			"year", currentYear)
	}
	return nil
}

// hasYear reports whether any holiday in hs falls in the given year.
func hasYear(hs []Holiday, year int) bool {
	for _, h := range hs {
		if h.Date.Year() == year {
			return true
		}
	}
	return false
}

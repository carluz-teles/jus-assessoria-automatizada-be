// Package calendar computes Brazilian judicial business days. It answers "is
// this a working day for a court?" and derives the deadline dates the
// acquisition and deadline slices depend on: published_at (1st business day
// after a DJEN disponibilização), deadline_start_at (1st business day after
// publication, CPC art. 224) and a deadline's end_date (AddBusinessDays).
//
// A day is NON-working when it is a weekend, inside the forensic recess
// (20 Dec–20 Jan inclusive, CPC art. 220 — a fixed rule, not table rows), or a
// registered holiday at the national / state (UF) / court scope. Holiday rows
// live in the `holiday` table behind the HolidaySource seam, so the pure
// business-day logic here is unit-tested without a database.
package calendar

import (
	"context"
	"fmt"
	"time"
)

// maxScan bounds every "walk forward until a business day" search. The longest
// legitimate run of non-working days is the recess plus its surrounding
// weekends/holidays (~a month); 400 days is comfortably beyond that, so hitting
// the bound means a broken HolidaySource, not a real calendar — surfaced as an
// error rather than an infinite loop.
const maxScan = 400

// HolidaySource reports whether a day is a registered holiday for the national
// calendar, a state (uf) or a court. It is the seam the Calendar depends on:
// production wires the Postgres-backed store (store.go); tests inject an
// in-memory fake, so the business-day math is verified without a database. An
// empty uf or court means "that scope does not apply" and the source must ignore
// it.
type HolidaySource interface {
	IsHoliday(ctx context.Context, day time.Time, uf, court string) (bool, error)
}

// Calendar derives judicial business days from a HolidaySource plus the fixed
// CPC rules (weekends + the art. 220 recess).
type Calendar struct {
	src HolidaySource
}

// New builds a Calendar over the given holiday source.
func New(src HolidaySource) *Calendar { return &Calendar{src: src} }

// IsBusinessDay reports whether day is a working day for a process at the given
// uf (comarca) and court. Non-working = Saturday/Sunday OR inside the art. 220
// recess OR a registered holiday at national/state/court scope. uf and court are
// optional ("" skips that scope). The weekend and recess checks are pure, so the
// source is consulted only when they pass.
func (c *Calendar) IsBusinessDay(ctx context.Context, day time.Time, uf, court string) (bool, error) {
	day = dateOnly(day)
	if isWeekend(day) || inRecess(day) {
		return false, nil
	}
	holiday, err := c.src.IsHoliday(ctx, day, uf, court)
	if err != nil {
		return false, fmt.Errorf("check holiday %s: %w", day.Format(time.DateOnly), err)
	}
	return !holiday, nil
}

// NextBusinessDay returns the first business day strictly after day.
func (c *Calendar) NextBusinessDay(ctx context.Context, day time.Time, uf, court string) (time.Time, error) {
	d := dateOnly(day)
	for i := 0; i < maxScan; i++ {
		d = d.AddDate(0, 0, 1)
		ok, err := c.IsBusinessDay(ctx, d, uf, court)
		if err != nil {
			return time.Time{}, err
		}
		if ok {
			return d, nil
		}
	}
	return time.Time{}, fmt.Errorf("no business day within %d days of %s", maxScan, dateOnly(day).Format(time.DateOnly))
}

// AddBusinessDays advances start by n business days and returns the resulting
// date plus the non-weekend non-working days skipped along the way (holidays and
// recess days), for the deadline's auditable holidays_applied. It follows CPC
// art. 224: the start day is EXCLUDED ("exclui-se o dia do começo") and the
// returned date is the n-th business day after it ("inclui-se o do vencimento").
// n must be >= 0; n == 0 returns start unchanged with no skips.
func (c *Calendar) AddBusinessDays(ctx context.Context, start time.Time, n int, uf, court string) (time.Time, []time.Time, error) {
	if n < 0 {
		return time.Time{}, nil, fmt.Errorf("AddBusinessDays: n must be >= 0, got %d", n)
	}
	d := dateOnly(start)
	if n == 0 {
		return d, nil, nil
	}

	var skipped []time.Time
	counted := 0
	for scan := 0; counted < n; scan++ {
		if scan >= maxScan+n {
			return time.Time{}, nil, fmt.Errorf("AddBusinessDays: exceeded %d-day scan from %s", maxScan+n, dateOnly(start).Format(time.DateOnly))
		}
		d = d.AddDate(0, 0, 1)
		ok, err := c.IsBusinessDay(ctx, d, uf, court)
		if err != nil {
			return time.Time{}, nil, err
		}
		switch {
		case ok:
			counted++
		case !isWeekend(d):
			// A holiday or recess day between start and end — recorded for the
			// auditable holidays_applied. Weekends are the norm, not audited.
			skipped = append(skipped, d)
		}
	}
	return d, skipped, nil
}

// dateOnly pins a value to midnight UTC, taking the year/month/day from the
// value's OWN components. The Calendar works on CIVIL dates, not instants: the
// contract is that callers pass a date whose Y/M/D already are the intended
// judicial date — a `date` column (pgx decodes it at UTC midnight) or a
// wall-clock time in the correct zone. A zoned instant that crosses midnight
// versus its civil date (e.g. a São Paulo evening stored as next-day UTC) must
// be normalized by the caller BEFORE it reaches the Calendar; dateOnly does not
// guess a timezone. In practice the sources are date-only (DJEN
// data_disponibilizacao, the `date` columns), so this is a no-op that just drops
// any stray time-of-day and keeps weekday/recess/equality checks stable.
func dateOnly(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// isWeekend reports Saturday or Sunday.
func isWeekend(d time.Time) bool {
	wd := d.Weekday()
	return wd == time.Saturday || wd == time.Sunday
}

// inRecess reports the forensic recess of CPC art. 220: 20 December through
// 20 January, inclusive. It is a fixed month/day rule independent of year — the
// span wraps the new year, so it matches December from the 20th on and January
// through the 20th.
func inRecess(d time.Time) bool {
	m, day := d.Month(), d.Day()
	return (m == time.December && day >= 20) || (m == time.January && day <= 20)
}

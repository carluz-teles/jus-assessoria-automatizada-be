package calendar

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeFetcher returns canned national holidays (or errors) per year.
type fakeFetcher struct {
	byYear map[int][]Holiday
	errFor map[int]error
}

func (f fakeFetcher) NationalHolidays(_ context.Context, year int) ([]Holiday, error) {
	if err := f.errFor[year]; err != nil {
		return nil, err
	}
	return f.byYear[year], nil
}

// fakeSeedStore records upserts and reports pre-seeded years.
type fakeSeedStore struct {
	seeded     map[int]bool
	seededErr  error
	upsertErr  error
	upserted   []Holiday
	upsertCall int
}

func (s *fakeSeedStore) SeededNationalYears(context.Context) (map[int]bool, error) {
	if s.seededErr != nil {
		return nil, s.seededErr
	}
	if s.seeded == nil {
		return map[int]bool{}, nil
	}
	return s.seeded, nil
}

func (s *fakeSeedStore) UpsertHolidays(_ context.Context, hs []Holiday) (int, error) {
	s.upsertCall++
	if s.upsertErr != nil {
		return 0, s.upsertErr
	}
	s.upserted = append(s.upserted, hs...)
	return len(hs), nil
}

func nat(dateStr string) Holiday {
	return Holiday{Scope: ScopeNational, Date: d(dateStr), Name: "feriado " + dateStr}
}

// upsertedYears returns the distinct years present in the recorded upserts.
func (s *fakeSeedStore) upsertedYears() map[int]bool {
	years := map[int]bool{}
	for _, h := range s.upserted {
		years[h.Date.Year()] = true
	}
	return years
}

func TestSeedNational_SkipsSeededFetchesMissing(t *testing.T) {
	t.Parallel()
	store := &fakeSeedStore{seeded: map[int]bool{2026: true}}
	fetcher := fakeFetcher{byYear: map[int][]Holiday{
		2027: {nat("2027-01-01")},
		2028: {nat("2028-01-01")},
	}}

	if err := SeedNational(context.Background(), store, fetcher, discardLogger(), 2026, 2); err != nil {
		t.Fatalf("SeedNational: %v", err)
	}

	got := store.upsertedYears()
	if got[2026] {
		t.Error("2026 already seeded, must not be re-upserted")
	}
	if !got[2027] || !got[2028] {
		t.Errorf("missing years not seeded: got %v, want 2027 and 2028", got)
	}
}

func TestSeedNational_ContinuesOnFetchError(t *testing.T) {
	t.Parallel()
	store := &fakeSeedStore{}
	fetcher := fakeFetcher{
		byYear: map[int][]Holiday{2026: {nat("2026-01-01")}, 2028: {nat("2028-01-01")}},
		errFor: map[int]error{2027: errors.New("upstream 503")},
	}

	// A failed year must not abort the seed nor return an error (fail-soft).
	if err := SeedNational(context.Background(), store, fetcher, discardLogger(), 2026, 2); err != nil {
		t.Fatalf("SeedNational must be fail-soft on fetch error, got: %v", err)
	}

	got := store.upsertedYears()
	if !got[2026] || !got[2028] {
		t.Errorf("good years not seeded: got %v", got)
	}
	if got[2027] {
		t.Error("failed year 2027 must not be seeded")
	}
}

func TestSeedNational_CurrentYearMissingIsSoft(t *testing.T) {
	t.Parallel()
	store := &fakeSeedStore{}
	// Total outage: every fetch fails, and nothing was pre-seeded.
	fetcher := fakeFetcher{errFor: map[int]error{2026: errors.New("down"), 2027: errors.New("down")}}

	// Must NOT fail boot even when the current year cannot be ensured.
	if err := SeedNational(context.Background(), store, fetcher, discardLogger(), 2026, 1); err != nil {
		t.Fatalf("SeedNational must not fail boot on total outage, got: %v", err)
	}
	if len(store.upserted) != 0 {
		t.Errorf("nothing should be upserted on total outage, got %d rows", len(store.upserted))
	}
}

func TestSeedNational_PropagatesStoreErrors(t *testing.T) {
	t.Parallel()
	fetcher := fakeFetcher{byYear: map[int][]Holiday{2026: {nat("2026-01-01")}}}

	t.Run("seeded-years read fails", func(t *testing.T) {
		t.Parallel()
		store := &fakeSeedStore{seededErr: errors.New("db down")}
		if err := SeedNational(context.Background(), store, fetcher, discardLogger(), 2026, 0); err == nil {
			t.Fatal("expected error when reading seeded years fails")
		}
	})

	t.Run("upsert fails", func(t *testing.T) {
		t.Parallel()
		store := &fakeSeedStore{upsertErr: errors.New("db down")}
		if err := SeedNational(context.Background(), store, fetcher, discardLogger(), 2026, 0); err == nil {
			t.Fatal("expected error when upsert fails")
		}
	})
}

func TestSeedNational_RejectsNegativeYearsAhead(t *testing.T) {
	t.Parallel()
	store := &fakeSeedStore{}
	if err := SeedNational(context.Background(), store, fakeFetcher{}, discardLogger(), 2026, -1); err == nil {
		t.Fatal("expected error for negative yearsAhead")
	}
}

// guard: the fixture dates parse and land in the expected year.
func TestNat(t *testing.T) {
	t.Parallel()
	h := nat("2027-01-01")
	if h.Scope != ScopeNational || h.Date.Year() != 2027 {
		t.Fatalf("nat() = %+v", h)
	}
	_ = time.January
}

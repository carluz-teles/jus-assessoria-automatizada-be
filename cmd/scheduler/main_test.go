package main

import (
	"testing"
	"time"
)

// TestDaysToIngest checks the daily window: yesterday back through the lookback,
// oldest first, date-only in UTC.
func TestDaysToIngest(t *testing.T) {
	t.Parallel()

	// now = 2025-08-10 14:33Z → yesterday = 08-09; lookback 3 → 08-06..08-09.
	now := time.Date(2025, 8, 10, 14, 33, 0, 0, time.UTC)
	got := daysToIngest(now, 3)

	want := []time.Time{
		time.Date(2025, 8, 6, 0, 0, 0, 0, time.UTC),
		time.Date(2025, 8, 7, 0, 0, 0, 0, time.UTC),
		time.Date(2025, 8, 8, 0, 0, 0, 0, time.UTC),
		time.Date(2025, 8, 9, 0, 0, 0, 0, time.UTC),
	}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if !got[i].Equal(want[i]) {
			t.Errorf("day[%d] = %s, want %s", i, got[i].Format(dayLayout), want[i].Format(dayLayout))
		}
	}
}

// TestDaysToIngest_ZeroLookback returns just yesterday.
func TestDaysToIngest_ZeroLookback(t *testing.T) {
	t.Parallel()

	now := time.Date(2025, 8, 10, 0, 0, 0, 0, time.UTC)
	got := daysToIngest(now, 0)
	if len(got) != 1 || !got[0].Equal(time.Date(2025, 8, 9, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("got %v, want [2025-08-09]", got)
	}
}

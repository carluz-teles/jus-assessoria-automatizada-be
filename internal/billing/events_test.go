package billing

import (
	"testing"
	"time"
)

// TestNewTrialEndingSoonCheck_ProcessAt covers the check's ETA math: it fires
// trialEndingSoonLeadDays before start-of-day(trial_ends_at), mirroring how the
// deadline slice's D-N marks anchor to start-of-day(end_date).
func TestNewTrialEndingSoonCheck_ProcessAt(t *testing.T) {
	t.Parallel()

	// A time-of-day component on trial_ends_at must not leak into the ETA — the
	// check always fires at midnight.
	trialEndsAt := time.Date(2026, 3, 15, 14, 30, 0, 0, time.UTC)
	want := time.Date(2026, 3, 13, 0, 0, 0, 0, time.UTC) // 15th minus 2 days, start of day

	check := newTrialEndingSoonCheck("tenant-1", "sub-1", trialEndsAt)

	at, ok := check.ProcessAt()
	if !ok {
		t.Fatal("ProcessAt() ok = false, want true (the check opts into future delivery)")
	}
	if !at.Equal(want) {
		t.Fatalf("ProcessAt() = %v, want %v", at, want)
	}
}

// TestNewTrialEndingSoonCheck_IdempotencyKey covers the STABLE per-subscription
// key: it must depend only on the subscription id, not on the trial_ends_at value
// or any other varying field, so a re-derive of the same subscription's check
// never mints a second asynq TaskID nor a second processed_event dedup key.
func TestNewTrialEndingSoonCheck_IdempotencyKey(t *testing.T) {
	t.Parallel()

	a := newTrialEndingSoonCheck("tenant-1", "sub-1", time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC))
	b := newTrialEndingSoonCheck("tenant-1", "sub-1", time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC))

	want := "trial-ending-soon:sub-1"
	if a.IdempotencyKey() != want {
		t.Fatalf("IdempotencyKey() = %q, want %q", a.IdempotencyKey(), want)
	}
	if a.IdempotencyKey() != b.IdempotencyKey() {
		t.Fatalf("idempotency key varied with trial_ends_at: %q vs %q, want the same stable key", a.IdempotencyKey(), b.IdempotencyKey())
	}

	other := newTrialEndingSoonCheck("tenant-1", "sub-2", time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC))
	if other.IdempotencyKey() == a.IdempotencyKey() {
		t.Fatalf("distinct subscriptions minted the same idempotency key: %q", a.IdempotencyKey())
	}
}

// TestNewTrialEndingSoon_FreshEventID covers the REAL event's id: unlike the
// check, it must be a fresh id per call — it is a genuinely new fact each time it
// fires, not a schedule-once mark.
func TestNewTrialEndingSoon_FreshEventID(t *testing.T) {
	t.Parallel()

	trialEndsAt := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)
	a := newTrialEndingSoon("tenant-1", trialEndsAt, 2)
	b := newTrialEndingSoon("tenant-1", trialEndsAt, 2)

	if a.IdempotencyKey() == "" {
		t.Fatal("IdempotencyKey() empty, want a minted event id")
	}
	if a.IdempotencyKey() == b.IdempotencyKey() {
		t.Fatalf("two calls minted the same event id %q, want fresh ids", a.IdempotencyKey())
	}
	if a.TenantID != "tenant-1" || a.DaysLeft != 2 || !a.TrialEndsAt.Equal(trialEndsAt) {
		t.Fatalf("payload = %+v, want tenant-1/2 days/%v", a, trialEndsAt)
	}
}

// TestDaysUntil covers the shared calendar-day math both OnTrialEndingSoonCheck's
// re-check and the subscription read model's trial_status use — start-of-day
// boundaries, so a few hours' difference never rounds to the wrong day.
func TestDaysUntil(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 10, 23, 0, 0, 0, time.UTC) // late in the day

	tests := []struct {
		name string
		t    time.Time
		want int
	}{
		{name: "same day", t: time.Date(2026, 1, 10, 0, 30, 0, 0, time.UTC), want: 0},
		{name: "tomorrow, even a minute past midnight", t: time.Date(2026, 1, 11, 0, 1, 0, 0, time.UTC), want: 1},
		{name: "5 days out", t: time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC), want: 5},
		{name: "yesterday (past)", t: time.Date(2026, 1, 9, 0, 0, 0, 0, time.UTC), want: -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := daysUntil(tt.t, now); got != tt.want {
				t.Errorf("daysUntil(%v, %v) = %d, want %d", tt.t, now, got, tt.want)
			}
		})
	}
}

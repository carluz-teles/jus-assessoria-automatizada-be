package onboarding

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jusassessoria/platform/internal/onboarding/onboardingdb"
)

func TestProgressToEntity(t *testing.T) {
	t.Run("no step activated, never dismissed", func(t *testing.T) {
		got := progressToEntity(onboardingdb.GetProgressRow{})
		want := Progress{}
		if got != want {
			t.Errorf("progressToEntity() = %+v, want %+v", got, want)
		}
	})

	t.Run("every step activated and dismissed", func(t *testing.T) {
		dismissed := time.Date(2026, 8, 27, 9, 30, 0, 0, time.UTC)
		got := progressToEntity(onboardingdb.GetProgressRow{
			SourcesConnected: true,
			MembersInvited:   true,
			FirstTriagem:     true,
			FirstAnalise:     true,
			FirstPeca:        true,
			DismissedAt:      pgtype.Timestamptz{Time: dismissed, Valid: true},
		})
		wantSteps := Steps{
			SourcesConnected: true,
			MembersInvited:   true,
			FirstTriagem:     true,
			FirstAnalise:     true,
			FirstPeca:        true,
		}
		if got.Steps != wantSteps {
			t.Errorf("Steps = %+v, want %+v", got.Steps, wantSteps)
		}
		if got.DismissedAt == nil || !got.DismissedAt.Equal(dismissed) {
			t.Errorf("DismissedAt = %v, want %v", got.DismissedAt, dismissed)
		}
	})

	t.Run("mixed steps map field-by-field, not all-or-nothing", func(t *testing.T) {
		got := progressToEntity(onboardingdb.GetProgressRow{
			SourcesConnected: true,
			FirstAnalise:     true,
		})
		wantSteps := Steps{SourcesConnected: true, FirstAnalise: true}
		if got.Steps != wantSteps {
			t.Errorf("Steps = %+v, want %+v", got.Steps, wantSteps)
		}
		if got.DismissedAt != nil {
			t.Errorf("DismissedAt = %v, want nil", got.DismissedAt)
		}
	})
}

func TestTimeToPtr(t *testing.T) {
	if got := timeToPtr(pgtype.Timestamptz{}); got != nil {
		t.Errorf("timeToPtr(invalid) = %v, want nil", got)
	}

	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	got := timeToPtr(pgtype.Timestamptz{Time: ts, Valid: true})
	if got == nil || !got.Equal(ts) {
		t.Errorf("timeToPtr(valid) = %v, want %v", got, ts)
	}
}

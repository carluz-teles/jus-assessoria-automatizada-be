package onboarding

import (
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jusassessoria/platform/internal/onboarding/onboardingdb"
)

// mapper.go is the boundary where driver types die (docs/erd-backend.md
// §4b.3): pgtype.* is absorbed here so the entity stays pure. The repository
// returns Progress, never the sqlc row.

// progressToEntity maps the GetProgress read-model row to Progress. The five
// booleans come straight off EXISTS subqueries (never NULL); only
// dismissed_at is nullable.
func progressToEntity(r onboardingdb.GetProgressRow) Progress {
	return Progress{
		Steps: Steps{
			SourcesConnected: r.SourcesConnected,
			MembersInvited:   r.MembersInvited,
			FirstTriagem:     r.FirstTriagem,
			FirstAnalise:     r.FirstAnalise,
			FirstPeca:        r.FirstPeca,
		},
		DismissedAt: timeToPtr(r.DismissedAt),
	}
}

// timeToPtr collapses a nullable timestamptz to a *time.Time, nil standing in
// for SQL NULL — dismissed_at is unset until the caller dismisses the widget.
func timeToPtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	return &t.Time
}

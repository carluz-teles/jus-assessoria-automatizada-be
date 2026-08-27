// Package onboarding is the vertical slice for the post-signup activation
// widget: a floating checklist that shows the escritório's real activation
// progress (sources connected, team invited, first triagem, first análise,
// first peça) and whether the caller dismissed it. It is read-only over other
// slices' tables (integration, membership, intimation, process_activity_log)
// via its OWN SQL in internal/onboarding/queries — it never imports another
// slice's entity or repository (docs/erd-backend.md, vertical-slice rule).
package onboarding

import "time"

// Steps is the 5-boolean activation checklist rendered by the widget. Every
// field is tenant-wide (any teammate's action satisfies it), never per-user.
type Steps struct {
	SourcesConnected bool
	MembersInvited   bool
	FirstTriagem     bool
	FirstAnalise     bool
	FirstPeca        bool
}

// Progress is the read model behind GET /v1/onboarding/progress: the caller's
// tenant-wide activation Steps plus the caller's OWN dismissal timestamp.
// Dismissal is per app_user, not per tenant — each teammate can dismiss the
// widget independently. DismissedAt is nil until the caller dismisses it.
type Progress struct {
	Steps       Steps
	DismissedAt *time.Time
}

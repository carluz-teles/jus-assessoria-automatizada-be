package deadline

import "github.com/jusassessoria/platform/lib/apperr"

// Typed, HTTP-agnostic slice errors. This domain is async (a listener, not an HTTP
// edge), so the Kind mostly drives logs and the asynq retry decision rather than a
// status code. Absence is always a typed error from the repository, never (nil, nil).
var (
	// ErrCourtRecordNotFound — the event's court_record_id resolves to no row in the
	// tenant. It should not happen (the producer emits the id it just wrote), so it
	// surfaces as a typed not-found rather than being swallowed.
	ErrCourtRecordNotFound = apperr.NewNotFound("court record not found")

	// ErrRuleNotFound — no active deadline_rule matched, not even the '*' catch-all.
	// The 0024 seed guarantees a catch-all, so this signals a missing/broken seed
	// (a config fault), not a normal path. Typed, never (nil, nil).
	ErrRuleNotFound = apperr.NewNotFound("deadline rule not found")

	// ErrDeadlineExists — a deadline already exists for the intimação (the 1:1
	// notification_id UNIQUE). The idempotent INSERT ... ON CONFLICT DO NOTHING yields
	// no row; the use case treats this as a no-op (the prazo is already there), so a
	// second observed for the same intimação never opens a phantom prazo.
	ErrDeadlineExists = apperr.NewConflict("deadline already exists for intimation")
)

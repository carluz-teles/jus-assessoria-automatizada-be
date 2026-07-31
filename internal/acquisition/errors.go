package acquisition

import "github.com/jusassessoria/platform/lib/apperr"

// Typed, HTTP-agnostic slice errors. The edge (lib/httpx) maps each apperr.Kind
// to a status; the domain and repository only ever see these values.
var (
	// ErrIntegrationNotFound — no integration for a (tenant, source). The
	// repository returns it instead of (nil, nil); the activation use case treats
	// it as "first activation", not a client-facing 404.
	ErrIntegrationNotFound = apperr.NewNotFound("integration not found")

	// ErrConnectorNotFound — the orchestrator has no connector registered for a
	// source. Surfaces at composition time (a misconfigured worker), not per event.
	ErrConnectorNotFound = apperr.NewNotFound("no connector registered for source")

	// ErrBackfillJobNotFound — no backfill_job matched the (tenant, id) of a slice
	// increment: the row is invisible under this tenant (RLS/tenant mismatch) or no
	// longer exists. The completion counter treats it as a no-op ack, never a retry.
	ErrBackfillJobNotFound = apperr.NewNotFound("backfill job not found")

	// ErrSyncRunNotFound — no sync_run matched an event_id lookup. On a re-delivery
	// of an already-marked event the sync use case treats it defensively as a
	// closed/absent run: a no-op ack, never a reopen.
	ErrSyncRunNotFound = apperr.NewNotFound("sync run not found")

	// ErrProcessLimitReached — the tenant is already at its plan's
	// active_process_limit (the v0 billing entitlement), so the sync cycle must not
	// create a NEW court record (→ 403). It surfaces ONLY on the MISS path of
	// FindOrCreateCourtRecord; a reobservation of an existing ACTIVE record is never
	// gated. The sync use case catches it, logs, and skips that item — the cycle
	// still closes OK (the block is expected, not a failure). A tenant with no
	// subscription resolves to limit 0 upstream (fail-closed), so this fires for it
	// on the first new record.
	ErrProcessLimitReached = apperr.NewForbidden("active process limit reached")
)

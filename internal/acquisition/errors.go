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

	// ErrParserNotFound — no registered parser CanParse a fetched payload (a
	// ParserSet with no member for that source). Like ErrConnectorNotFound it is a
	// misconfigured composition; the sync use case handles it as a parse fault.
	ErrParserNotFound = apperr.NewNotFound("no parser registered for payload")

	// ErrNoParserForPayload — the composite parser holds no member that CanParse a
	// payload, i.e. a source is wired with a connector but no matching parser. It is
	// a composition-time misconfiguration surfacing per event; the sync use case
	// treats a parse failure as terminal, so a bad wiring archives the task rather
	// than burning retries.
	ErrNoParserForPayload = apperr.NewNotFound("no parser registered for payload")

	// ErrBackfillJobNotFound — no backfill_job matched the (tenant, id) of a slice
	// increment: the row is invisible under this tenant (RLS/tenant mismatch) or no
	// longer exists. The completion counter treats it as a no-op ack, never a retry.
	ErrBackfillJobNotFound = apperr.NewNotFound("backfill job not found")

	// ErrIntimationNotFound — no intimation matched the (id, tenant) of a GET
	// /v1/intimacoes/:id deep-link: the row is unknown or belongs to another tenant
	// (invisible under this tenant's scope). The read repo returns it instead of
	// (nil, nil) so the handler answers a typed 404, never a 500 or an empty 200.
	ErrIntimationNotFound = apperr.NewNotFound("intimação não encontrada")

	// ErrProcessoNotFound — no court record matched the (id, tenant) of a GET
	// /v1/processos/:id deep-link: the row is unknown or belongs to another tenant
	// (invisible under this tenant's scope). The read repo returns it instead of
	// (nil, nil) so the handler answers a typed 404, never a 500 or an empty 200.
	ErrProcessoNotFound = apperr.NewNotFound("processo não encontrado")

	// ErrResponsibleNotMember — the user_id a PUT /processos/:id/responsavel names is
	// not an app_user of the caller's escritório (a foreign or unknown user). It is
	// invalid client input (→ 400), not a 404 on the process: the process is real, the
	// proposed responsável is not one of ours. Guards against pinning a process on
	// someone outside the tenant. Only checked for a non-null assignee (desatribuir skips).
	ErrResponsibleNotMember = apperr.NewInvalid("usuário não é membro do escritório")

	// ErrSyncRunNotFound — no sync_run matched an event_id lookup. On a re-delivery
	// of an already-marked event the sync use case treats it defensively as a
	// closed/absent run: a no-op ack, never a reopen.
	ErrSyncRunNotFound = apperr.NewNotFound("sync run not found")

	// ErrCourtRecordNotFound — the court_record an in-place grade targets vanished
	// between the fetch and the UPDATE (deleted, or invisible under this tenant's
	// RLS). UpdateCourtRecordGrade returns it instead of (nil, nil) so the enrichment
	// never silently drops a grade; in practice the per-tenant write lock makes it
	// unreachable (the row was just observed/looked up in the same tx).
	ErrCourtRecordNotFound = apperr.NewNotFound("court record not found")

	// ErrProcessLimitReached — the tenant is already at its plan's
	// active_process_limit (the v0 billing entitlement), so the sync cycle must not
	// create a NEW court record (→ 403). It surfaces ONLY on the MISS path of
	// FindOrCreateCourtRecord; a reobservation of an existing ACTIVE record is never
	// gated. The sync use case catches it, logs, and skips that item — the cycle
	// still closes OK (the block is expected, not a failure). A tenant with no
	// subscription resolves to limit 0 upstream (fail-closed), so this fires for it
	// on the first new record.
	ErrProcessLimitReached = apperr.NewForbidden("active process limit reached")

	// ErrOABNotFound — no DJEN communication in the lookup window names this OAB.
	// Best-effort: it does NOT mean the OAB is invalid, only that we found no
	// recent publication to read the holder's name from. The caller (onboarding,
	// Termos) falls back to a placeholder rather than blocking on this.
	ErrOABNotFound = apperr.NewNotFound("no recent DJEN communication names this OAB")

	// ErrActivationBlocked — the tenant is already at (or above) its plan's
	// active_process_limit at the moment it tries to ACTIVATE a source (→ 403). A
	// sibling of ErrProcessLimitReached, not a reuse of it: that one blocks a single
	// new court record mid-sync-cycle and lets the cycle close OK; this one blocks
	// the WHOLE activation request up front, before any integration row is upserted
	// or an integration_activated event (and the backfill it triggers) is ever
	// published — the ERD's edge-of-the-API entitlement gate, so a maxed-out tenant
	// never waits on a backfill of thousands of processes the worker's gate would
	// mostly discard anyway. A tenant with no subscription resolves to limit 0
	// upstream (fail-closed), so its very first activation is blocked.
	ErrActivationBlocked = apperr.NewForbidden("tenant already at active process limit; upgrade the plan before activating a new source")
)

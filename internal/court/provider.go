package court

import (
	"context"
	"time"

	"github.com/jusassessoria/platform/lib/eproc"
)

// Session is the opaque, persistable snapshot of a provider's authenticated
// session — a light alias (not a fresh type) over lib/eproc's own Session so the
// interface stays rename-friendly if a future adapter needs a different shape,
// without inventing an abstraction only one real implementation exercises yet.
type Session = eproc.Session

// CourtProvider is the per-(court,system) adapter the ERD's "Adapter por sistema,
// core agnóstico" principle asks for: the use case never imports lib/eproc, an
// e-SAJ client, or any portal-specific transport directly — only this interface.
type CourtProvider interface {
	// Connect establishes (or confirms) an authenticated session for conn, using
	// whatever mechanism THIS system needs — password form, mutual TLS, TOTP
	// submission, all hidden inside the adapter. seed is the decrypted TOTP seed
	// (from conn.MFASeedRef), "" when none is on file yet; the adapter must not
	// persist it beyond this call.
	//
	// Returns ErrMFAEnrollmentRequired when the portal demanded a second factor
	// this connection has no seed for — the use case reacts by running
	// EnrollMFA (if the provider supports it) and retrying once, never in a loop.
	Connect(ctx context.Context, conn *CourtConnection, seed string) error

	// FetchAutos pulls one process's docket for conn, priming a fresh underlying
	// session from sessionIn (skipping a login round-trip when it's still valid
	// server-side) and ALWAYS returning the resulting session — even on error — so
	// the caller can persist it and thread it into the next call. This IS the
	// session-reuse mechanic FetchAutosUseCase relies on: a login only happens on
	// genuine staleness (the adapter's own re-auth-on-rejection), never once per
	// call just because a fresh client object was built.
	//
	// A REAUTH_REQUIRED-shaped failure (session dead, credential still good) is
	// handled INSIDE the adapter transparently — it surfaces to the caller only
	// when even a fresh login fails (apperr Unauthorized/Forbidden: the credential
	// itself is broken, not just the session), which the use case treats as "this
	// connection needs attention", never as a per-item retry.
	//
	// courtRecordID tags any document downloaded this call with the right FK (the
	// document slice's court_record_id). docketCursor is the incremental cut:
	// only events strictly newer than it are new since the last fetch, so only
	// THEIR documents get downloaded — eproc's own event log stays a stable,
	// authoritative source for "which documents exist and where", never a second
	// andamento/timeline store (that stays DJEN's job, see internal/acquisition's
	// docket_entry — DJEN is the lower-risk, nationally-standardized source for
	// the movement TIMELINE; eproc here is purely the document ACCESS mechanism).
	// Zero docketCursor means "never fetched" — every event's documents count.
	FetchAutos(ctx context.Context, conn *CourtConnection, seed string, sessionIn Session, courtRecordID, cnjNumber string, docketCursor time.Time) (AutosResult, Session, error)
}

// DocumentWriter is the narrow port FetchAutos uses to hand a downloaded
// document's bytes into the EXISTING document/indexing pipeline (the SAME one
// manual uploads already go through — extraction → OCR → chunk → embedding).
// Generic types only (no internal/document import here — see
// cmd/worker-court's documentWriterAdapter, the only place allowed to know
// about both slices, same technique as courtCertSignerFunc for the
// certificate). Returns the created document's id (informational — nothing in
// this slice currently reads it back).
type DocumentWriter interface {
	WriteDocument(ctx context.Context, tenantID, courtRecordID, mimeType, checksum, title string, data []byte) (documentID string, err error)
}

// AutosResult is what a successful FetchAutos call surfaces: the process
// itself, its docket entries (kept in-memory only — never persisted as a
// competing andamento store, see FetchAutos's doc), and how many NEW
// documents got downloaded+handed to DocumentWriter this call (telemetry only
// — the document rows themselves are the durable record).
type AutosResult struct {
	Process             eproc.Process
	Events              []eproc.Event
	DocumentsDownloaded int
	// LatestCursor is max(Events[i].Date) — the incremental cursor
	// FetchAutosUseCase advances court_fetch_state.docket_cursor to. Zero when
	// Events is empty (no docket activity found this fetch).
	LatestCursor time.Time
}

// MFAEnroller is an OPTIONAL capability: adapters for systems requiring TOTP
// implement it; a provider that doesn't need MFA at all (or doesn't support
// automating it yet) simply doesn't implement this interface, and the use case
// checks via a type assertion (`provider.(MFAEnroller)`) — the same "narrow,
// checked interface" idiom as io.ReaderAt, never a required method every adapter
// must stub out.
type MFAEnroller interface {
	// EnrollMFA authenticates (without a seed — this only runs BEFORE one exists)
	// and drives the portal's own "enable two-factor" flow, returning the raw
	// RFC 6238 secret it captured. The adapter never persists it; the use case
	// seals it into the vault immediately after this call returns.
	EnrollMFA(ctx context.Context, conn *CourtConnection) (seed string, err error)
}

// ErrMFAEnrollmentRequired is what a CourtProvider.Connect returns to signal "this
// portal wants a second factor and conn has no seed to try" — distinct from any
// other Connect failure so the use case knows to attempt automated enrollment
// rather than just surfacing an error.
var ErrMFAEnrollmentRequired = errMFAEnrollmentRequired{}

type errMFAEnrollmentRequired struct{}

func (errMFAEnrollmentRequired) Error() string {
	return "court: portal requires MFA and no seed is on file for this connection"
}

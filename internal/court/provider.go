package court

import "context"

// CourtProvider is the per-(court,system) adapter the ERD's "Adapter por sistema,
// core agnóstico" principle asks for: the use case never imports lib/eproc, an
// e-SAJ client, or any portal-specific transport directly — only this interface.
// Scoped to authentication only in this fatia; FetchAutos/Filing join this
// interface in later fatias (ERD §6/§7), when there's a real second capability to
// design against — adding methods speculatively now would be guessing at a shape
// nothing exercises yet.
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

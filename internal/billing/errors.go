package billing

import "github.com/jusassessoria/platform/lib/apperr"

// Typed, HTTP-agnostic slice errors. The edge (lib/httpx) maps each apperr.Kind
// to a status; the domain and repository only ever see these values. Absence is
// a typed error from the repository, never (nil, nil).
//
// Every non-2xx the webhook returns makes Stripe retry the delivery (its at-
// least-once policy). That is deliberate for the transient/ordering cases below
// (a status event racing ahead of the create, or metadata not yet propagated):
// a retry lets the projection catch up rather than dropping the event.
var (
	// ErrInvalidSignature — the Stripe-Signature header did not verify against the
	// signing secret (→ 400). A forged or misconfigured caller, refused before any
	// effect. Kept as an Invalid (not Unauthorized) per the slice contract: a bad
	// signature is a malformed request at this public edge.
	ErrInvalidSignature = apperr.NewInvalid("invalid stripe signature")
	// ErrMalformedEvent — the signed payload verified but its object could not be
	// decoded into the shape the type promises (→ 400).
	ErrMalformedEvent = apperr.NewInvalid("malformed stripe event payload")
	// ErrMissingTenant — a subscription event carried no metadata.tenant_id, so we
	// cannot scope the write to a tenant (→ 400, which makes Stripe retry). The
	// checkout (fatia 4-B) sets this on the subscription; its absence is a data
	// fault a retry may resolve.
	ErrMissingTenant = apperr.NewInvalid("stripe event missing tenant_id metadata")
	// ErrSubscriptionNotFound — no local subscription for the given tenant or Stripe
	// customer (→ 404, which makes Stripe retry). Surfaces when a status event
	// (deleted, payment_failed) races ahead of customer.subscription.created.
	ErrSubscriptionNotFound = apperr.NewNotFound("subscription not found")
	// ErrPlanUnresolved — the subscription's price/product has no usable
	// active_process_limit in its Stripe metadata (→ 400). The catalog is
	// misconfigured; refuse rather than project a zero entitlement silently.
	ErrPlanUnresolved = apperr.NewInvalid("stripe product metadata missing active_process_limit")
)

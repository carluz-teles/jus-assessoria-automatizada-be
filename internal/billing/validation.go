package billing

import validation "github.com/go-ozzo/ozzo-validation/v4"

// CheckoutRequest is the POST /v1/billing/checkout body: the Stripe price the
// tenant is subscribing to. tenant_id is NOT here — it comes from the verified
// principal (TenantFromCtx), never the body.
type CheckoutRequest struct {
	PriceID string `json:"price_id"`
}

// Validate enforces the one boundary rule via ozzo (method-based, not struct
// tags): price_id must be present. A failure here is a 400 at the edge
// (KindInvalid → 400); Stripe rejects an unknown/inactive id at session creation.
func (r CheckoutRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.PriceID, validation.Required),
	)
}

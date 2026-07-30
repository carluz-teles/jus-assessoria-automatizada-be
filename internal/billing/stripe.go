package billing

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	stripe "github.com/stripe/stripe-go/v86"
	"github.com/stripe/stripe-go/v86/webhook"

	"github.com/jusassessoria/platform/lib/apperr"
)

// metadataTenantID is the metadata key the checkout (fatia 4-B) stamps the tenant
// id under, on the Stripe objects this slice reads it back from.
const metadataTenantID = "tenant_id"

// productMetaActiveProcessLimit is the Stripe product metadata key holding the v0
// entitlement — the ACTIVE-process ceiling. productMetaPlan is the optional plan
// identifier; the product name is the fallback.
const (
	productMetaActiveProcessLimit = "active_process_limit"
	productMetaPlan               = "plan"
)

// stripeGateway is the concrete StripeGateway: the ONLY place the stripe-go SDK is
// imported (the domain depends on the port, not the SDK). It verifies webhooks
// with the signing secret and reads the plan catalog through an API client.
type stripeGateway struct {
	webhookSecret string
	client        *stripe.Client
}

var _ StripeGateway = (*stripeGateway)(nil)

// NewStripeGateway builds the gateway from the Stripe secret key (for the catalog
// API) and the webhook signing secret (for signature verification). Both arrive
// from config; an empty secret does not fail construction — it fails at the first
// verify/resolve, mirroring how the Clerk webhook secret is handled.
func NewStripeGateway(secretKey, webhookSecret string) StripeGateway {
	return &stripeGateway{
		webhookSecret: webhookSecret,
		client:        stripe.NewClient(secretKey),
	}
}

// VerifyWebhook verifies the Stripe-Signature over the RAW body and decodes the
// event into the SDK-agnostic StripeEvent. IgnoreAPIVersionMismatch is true so a
// difference between the account's API version and the pinned SDK version does not
// reject an otherwise valid, signed event; the tolerance window stays on for
// replay protection. A verification failure is ErrInvalidSignature; an
// undecodable known object, ErrMalformedEvent.
func (g *stripeGateway) VerifyWebhook(payload []byte, sigHeader string) (StripeEvent, error) {
	ev, err := webhook.ConstructEventWithOptions(payload, sigHeader, g.webhookSecret, webhook.ConstructEventOptions{
		IgnoreAPIVersionMismatch: true,
	})
	if err != nil {
		return StripeEvent{}, ErrInvalidSignature
	}

	out := StripeEvent{ID: ev.ID, Type: string(ev.Type)}

	switch ev.Type {
	case stripe.EventTypeCustomerSubscriptionCreated,
		stripe.EventTypeCustomerSubscriptionUpdated,
		stripe.EventTypeCustomerSubscriptionDeleted:
		var sub stripe.Subscription
		if err := json.Unmarshal(ev.Data.Raw, &sub); err != nil {
			return StripeEvent{}, ErrMalformedEvent
		}
		out.TenantID = sub.Metadata[metadataTenantID]
		out.Subscription = toStripeSubscription(&sub)

	case stripe.EventTypeInvoicePaymentFailed:
		var inv stripe.Invoice
		if err := json.Unmarshal(ev.Data.Raw, &inv); err != nil {
			return StripeEvent{}, ErrMalformedEvent
		}
		out.TenantID = inv.Metadata[metadataTenantID]
		out.Invoice = toStripeInvoice(&inv)
	}

	return out, nil
}

// ResolvePlan reads the plan and ACTIVE-process ceiling from the Stripe product
// behind a price. The product is expanded on the price retrieval so its metadata
// is populated in one round-trip. A missing/zero/non-numeric active_process_limit
// is ErrPlanUnresolved (the catalog is misconfigured); the plan is the product's
// `plan` metadata, falling back to its name.
func (g *stripeGateway) ResolvePlan(ctx context.Context, priceID string) (string, int, error) {
	params := &stripe.PriceRetrieveParams{}
	params.AddExpand("product")

	price, err := g.client.V1Prices.Retrieve(ctx, priceID, params)
	if err != nil {
		return "", 0, apperr.NewUnavailable("resolve stripe price", err)
	}
	if price.Product == nil {
		return "", 0, ErrPlanUnresolved
	}

	limit, err := strconv.Atoi(price.Product.Metadata[productMetaActiveProcessLimit])
	if err != nil || limit <= 0 {
		return "", 0, ErrPlanUnresolved
	}

	plan := price.Product.Metadata[productMetaPlan]
	if plan == "" {
		plan = price.Product.Name
	}
	return plan, limit, nil
}

// toStripeSubscription absorbs the SDK subscription shape into the neutral type:
// the customer id off the expandable Customer, and the price id + paid-through
// window off the first subscription item (v82+ moved current_period_end onto the
// items). Missing items/customer leave the respective fields zero — the domain
// only reads the price id on create/update, where it is always present.
func toStripeSubscription(sub *stripe.Subscription) *StripeSubscription {
	out := &StripeSubscription{
		ID:     sub.ID,
		Status: string(sub.Status),
	}
	if sub.Customer != nil {
		out.CustomerID = sub.Customer.ID
	}
	if sub.Items != nil && len(sub.Items.Data) > 0 {
		item := sub.Items.Data[0]
		if item.Price != nil {
			out.PriceID = item.Price.ID
		}
		if item.CurrentPeriodEnd > 0 {
			out.CurrentPeriodEnd = time.Unix(item.CurrentPeriodEnd, 0).UTC()
		}
	}
	return out
}

// toStripeInvoice absorbs the SDK invoice shape into the neutral type: the id, the
// amount due, and the customer id (the domain's fallback path to recover the
// tenant when the invoice carries no tenant_id metadata).
func toStripeInvoice(inv *stripe.Invoice) *StripeInvoice {
	out := &StripeInvoice{
		ID:        inv.ID,
		AmountDue: inv.AmountDue,
	}
	if inv.Customer != nil {
		out.CustomerID = inv.Customer.ID
	}
	return out
}

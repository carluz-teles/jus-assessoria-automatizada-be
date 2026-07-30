package billing

import (
	"context"
	"encoding/json"
	"errors"
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

// Idempotency keys make the write calls to Stripe safe to re-submit. The local
// subscription row that StartCheckout guards on is only projected LATER by the
// webhook, so within the checkout window (a double-click, two tabs, a fast FE
// retry) FindByTenant is still not-found and EnsureCustomer/CreateCheckoutSession
// would otherwise run twice with no protection — creating two customers/sessions
// that each bill the tenant. A deterministic key per tenant/operation makes Stripe
// return the object it already created instead of a duplicate.
//
// The keys carry NO clock and NO nonce so a retry always derives the same value.
// Stripe stores a key's result for 24h, which matches a Checkout Session's own
// validity: a genuine re-subscribe to the same price within that window returns the
// still-valid pending session (correct, not a duplicate); past it Stripe issues a
// fresh one.
func customerIdempotencyKey(tenantID string) string {
	return "ensure-customer:" + tenantID
}

func checkoutIdempotencyKey(tenantID, priceID string) string {
	return "checkout:" + tenantID + ":" + priceID
}

// mapStripeError classifies a stripe-go SDK error for the HTTP edge. An
// invalid_request_error — an unknown or inactive price_id, a malformed argument —
// is the CALLER's fault, so it becomes a 400 (apperr.NewInvalid) carrying Stripe's
// own explanation. Every other failure (a network error, a timeout, a rate limit,
// a 5xx, any other Stripe error type) stays a 503 (apperr.NewUnavailable): the
// provider is momentarily unreachable and the caller may retry. op is the stable,
// low-cardinality action label reused for both the message and the 503 log.
func mapStripeError(op string, err error) error {
	var se *stripe.Error
	if errors.As(err, &se) && se.Type == stripe.ErrorTypeInvalidRequest {
		return apperr.NewInvalid(op + ": " + se.Msg)
	}
	return apperr.NewUnavailable(op, err)
}

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
		return "", 0, mapStripeError("resolve stripe price", err)
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

// EnsureCustomer creates a Stripe Customer stamped with metadata.tenant_id (the
// key the webhook reads the tenant back from). It always creates: the reuse of an
// existing customer is the caller's decision (StartCheckout passes the stored id
// instead of calling this), so the gateway stays a thin, DB-unaware Stripe port.
func (g *stripeGateway) EnsureCustomer(ctx context.Context, tenantID string) (string, error) {
	params := &stripe.CustomerCreateParams{
		Metadata: map[string]string{metadataTenantID: tenantID},
	}
	// Deterministic per tenant: a re-submit before the webhook projects the row
	// reuses this key, so Stripe returns the existing customer instead of a second.
	params.SetIdempotencyKey(customerIdempotencyKey(tenantID))

	c, err := g.client.V1Customers.Create(ctx, params)
	if err != nil {
		return "", mapStripeError("create stripe customer", err)
	}
	return c.ID, nil
}

// CreateCheckoutSession opens a hosted Checkout in subscription mode for the
// customer/price and returns its redirect URL. tenant_id is stamped on
// subscription_data.metadata so the resulting subscription — and thus the 4-A
// webhook (customer.subscription.created) — carries the tenant scope; a positive
// TrialDays starts the subscription on a trial.
func (g *stripeGateway) CreateCheckoutSession(ctx context.Context, p CheckoutParams) (string, error) {
	subData := &stripe.CheckoutSessionCreateSubscriptionDataParams{
		Metadata: map[string]string{metadataTenantID: p.TenantID},
	}
	if p.TrialDays > 0 {
		subData.TrialPeriodDays = stripe.Int64(int64(p.TrialDays))
	}

	params := &stripe.CheckoutSessionCreateParams{
		Mode:       stripe.String(string(stripe.CheckoutSessionModeSubscription)),
		Customer:   stripe.String(p.CustomerID),
		SuccessURL: stripe.String(p.SuccessURL),
		CancelURL:  stripe.String(p.CancelURL),
		LineItems: []*stripe.CheckoutSessionCreateLineItemParams{
			{Price: stripe.String(p.PriceID), Quantity: stripe.Int64(1)},
		},
		SubscriptionData: subData,
	}
	// Deterministic per tenant+price: a double-click / two-tab / fast FE retry for
	// the same plan reuses the pending session instead of opening a second one that
	// would create a second subscription. A different price is a different key.
	params.SetIdempotencyKey(checkoutIdempotencyKey(p.TenantID, p.PriceID))

	session, err := g.client.V1CheckoutSessions.Create(ctx, params)
	if err != nil {
		return "", mapStripeError("create stripe checkout session", err)
	}
	return session.URL, nil
}

// CreatePortalSession opens a Billing Portal session for the customer and returns
// its redirect URL (where the tenant manages its subscription and invoices).
func (g *stripeGateway) CreatePortalSession(ctx context.Context, customerID, returnURL string) (string, error) {
	params := &stripe.BillingPortalSessionCreateParams{
		Customer:  stripe.String(customerID),
		ReturnURL: stripe.String(returnURL),
	}
	session, err := g.client.V1BillingPortalSessions.Create(ctx, params)
	if err != nil {
		return "", mapStripeError("create stripe portal session", err)
	}
	return session.URL, nil
}

// ListPlans reads the active plan catalog: active recurring prices with their
// product expanded in one round-trip. A price is a Plan only when its product is
// active and carries a usable active_process_limit — a misconfigured product is
// skipped (not fatal) so one bad catalog entry never hides the rest. The list
// auto-paginates via V1List.All.
func (g *stripeGateway) ListPlans(ctx context.Context) ([]Plan, error) {
	params := &stripe.PriceListParams{Active: stripe.Bool(true)}
	params.AddExpand("data.product")

	plans := []Plan{}
	for price, err := range g.client.V1Prices.List(ctx, params).All(ctx) {
		if err != nil {
			return nil, mapStripeError("list stripe prices", err)
		}
		plan, ok := toPlan(price)
		if !ok {
			continue
		}
		plans = append(plans, plan)
	}
	return plans, nil
}

// toPlan absorbs a Stripe price+product into a Plan, reporting ok=false for a
// catalog entry the checkout cannot use: no expanded/active product, no recurring
// interval, or a missing/zero active_process_limit. The name is the product's
// `plan` metadata, falling back to its display name (mirrors ResolvePlan).
func toPlan(price *stripe.Price) (Plan, bool) {
	prod := price.Product
	if prod == nil || !prod.Active || price.Recurring == nil {
		return Plan{}, false
	}

	limit, err := strconv.Atoi(prod.Metadata[productMetaActiveProcessLimit])
	if err != nil || limit <= 0 {
		return Plan{}, false
	}

	name := prod.Metadata[productMetaPlan]
	if name == "" {
		name = prod.Name
	}
	return Plan{
		PriceID:            price.ID,
		Name:               name,
		Amount:             price.UnitAmount,
		Interval:           string(price.Recurring.Interval),
		ActiveProcessLimit: limit,
	}, true
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

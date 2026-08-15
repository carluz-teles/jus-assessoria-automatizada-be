package billing

import (
	"context"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/httpx"
	"github.com/jusassessoria/platform/lib/httpx/middleware"
)

// roleAdmin is the product role allowed to manage billing. Subscribing, opening
// the portal and reading the plan catalog are all escritório-owner actions, so
// every billing route is ADMIN-only; a LAWYER gets 403. It is the wire-level
// string the auth middleware puts on the principal (identity.Role at the edge).
const roleAdmin = "ADMIN"

// handlerUC is the narrow port the Handler uses from the billing use case —
// exactly the four endpoint methods, so the handler never sees the webhook path.
type handlerUC interface {
	StartCheckout(ctx context.Context, tenantID, priceID string) (string, error)
	OpenPortal(ctx context.Context, tenantID string) (string, error)
	GetSubscription(ctx context.Context, tenantID string) (*Subscription, error)
	ListPlans(ctx context.Context) ([]StripePlan, error)
	// Now backs the subscription view's trial_status/days_until_trial_end fields
	// (fatia 2) — the read model computes "days left" against the SAME clock the
	// use case's trial math uses, never re-deriving "now" independently at the edge.
	Now() time.Time
}

// Handler is the billing HTTP surface for the authenticated /v1 endpoints. It owns
// its routing; the api only composes by calling RegisterV1. (The public Stripe
// webhook is a separate handler — WebhookHandler — mounted off the router root.)
type Handler struct {
	uc handlerUC
}

// NewHandler wires the handler to the billing use case. *UseCase satisfies
// handlerUC, so the api shares one use case between this and the webhook handler.
func NewHandler(uc handlerUC) *Handler {
	return &Handler{uc: uc}
}

// RegisterV1 mounts billing's authenticated routes on the /v1 group, every one
// guarded by RequireRole(ADMIN). tenant_id is read from the verified principal in
// each handler, never from the body — a caller only ever acts on its own tenant.
func (h *Handler) RegisterV1(r fiber.Router) {
	r.Post("/billing/checkout", middleware.RequireRole(roleAdmin), h.checkout)
	r.Post("/billing/portal", middleware.RequireRole(roleAdmin), h.portal)
	r.Get("/billing/subscription", middleware.RequireRole(roleAdmin), h.subscription)
	r.Get("/billing/plans", middleware.RequireRole(roleAdmin), h.plans)
}

// checkoutResponse / portalResponse carry the single hosted-session URL the client
// redirects the tenant to.
type checkoutResponse struct {
	CheckoutURL string `json:"checkout_url"`
}

type portalResponse struct {
	PortalURL string `json:"portal_url"`
}

// Trial status values the subscription view's TrialStatus field takes (fatia 2).
// Computed by newSubscriptionView against the use case's clock (Now), never
// stored — the subscription row only ever stores TrialEndsAt.
const (
	// trialStatusNotInTrial — the subscription is not (or no longer) trialing:
	// Status != trialing, regardless of any stored TrialEndsAt.
	trialStatusNotInTrial = "not_in_trial"
	// trialStatusActive — trialing, with more than trialEndingSoonLeadDays left.
	trialStatusActive = "trial_active"
	// trialStatusEndingSoon — trialing, trialEndingSoonLeadDays or fewer days left
	// (the same threshold TrialEndingSoonCheck fires at).
	trialStatusEndingSoon = "trial_ending_soon"
	// trialStatusExpired — trialing, but trial_ends_at already passed. KNOWN GAP
	// (undecided product behavior, not silently resolved here): nothing today
	// automatically transitions the subscription's Status off "trialing" when the
	// trial lapses without a conversion — this value is a hint computed purely at
	// READ time, not a stored state, and the row itself stays "trialing"
	// indefinitely until a real Stripe event (or a future reconciliation job)
	// flips it. See KNOWN_LIMITATIONS in the fatia's handoff.
	trialStatusExpired = "trial_expired"
)

// subscriptionView is the read model for GET /v1/billing/subscription — the fields
// the UI shows. The internal id and Stripe ids are deliberately absent: the client
// needs the plan, its lifecycle and entitlement, not the billing wiring.
//
// LocalPlanID is additive (fatia 5-A): the local plan (migration 0037) backing
// the projection, nil for a row not yet re-projected by a webhook since the
// catalog migrated. TrialEndsAt/DaysUntilTrialEnd/TrialStatus are additive
// (fatia 2): DaysUntilTrialEnd is nil whenever TrialStatus is not_in_trial (there
// is no trial window to count against).
type subscriptionView struct {
	Plan               string     `json:"plan"`
	Status             Status     `json:"status"`
	CurrentPeriodEnd   *time.Time `json:"current_period_end"`
	ActiveProcessLimit int        `json:"active_process_limit"`
	LocalPlanID        *string    `json:"local_plan_id"`
	TrialEndsAt        *time.Time `json:"trial_ends_at"`
	DaysUntilTrialEnd  *int       `json:"days_until_trial_end"`
	TrialStatus        string     `json:"trial_status"`
}

// planView is the read model for one entry of GET /v1/billing/plans. Amount is in
// the currency's minor unit (cents), mirroring Stripe.
type planView struct {
	PriceID            string `json:"price_id"`
	Name               string `json:"name"`
	Amount             int64  `json:"amount"`
	Interval           string `json:"interval"`
	ActiveProcessLimit int    `json:"active_process_limit"`
}

// plansEnvelope is the {data:[...]} response for the plan catalog. The set is tiny
// (a handful of plans), so it is returned whole — no cursor pagination.
type plansEnvelope struct {
	Data []planView `json:"data"`
}

// checkout handles POST /v1/billing/checkout: validates the body, starts a Stripe
// Checkout for the principal's tenant and returns the redirect URL. A tenant that
// already subscribes gets 409; an empty price_id, 400.
func (h *Handler) checkout(c *fiber.Ctx) error {
	var req CheckoutRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteError(c, apperr.NewInvalid("malformed request body"))
	}
	if err := req.Validate(); err != nil {
		return httpx.WriteValidationError(c, err)
	}

	tenantID := httpx.TenantFromCtx(c)
	url, err := h.uc.StartCheckout(c.UserContext(), tenantID, req.PriceID)
	if err != nil {
		return httpx.WriteError(c, err)
	}

	return c.Status(fiber.StatusOK).JSON(checkoutResponse{CheckoutURL: url})
}

// portal handles POST /v1/billing/portal: opens a Billing Portal session for the
// principal's tenant. A tenant with no Stripe customer yet gets 404.
func (h *Handler) portal(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	url, err := h.uc.OpenPortal(c.UserContext(), tenantID)
	if err != nil {
		return httpx.WriteError(c, err)
	}

	return c.Status(fiber.StatusOK).JSON(portalResponse{PortalURL: url})
}

// subscription handles GET /v1/billing/subscription: returns the tenant's local
// subscription read model (no Stripe call). A tenant that never checked out gets
// 404 (ErrSubscriptionNotFound).
func (h *Handler) subscription(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	sub, err := h.uc.GetSubscription(c.UserContext(), tenantID)
	if err != nil {
		return httpx.WriteError(c, err)
	}

	return c.Status(fiber.StatusOK).JSON(newSubscriptionView(sub, h.uc.Now()))
}

// plans handles GET /v1/billing/plans: returns the active plan catalog read from
// Stripe, wrapped in the {data:[...]} envelope.
func (h *Handler) plans(c *fiber.Ctx) error {
	plans, err := h.uc.ListPlans(c.UserContext())
	if err != nil {
		return httpx.WriteError(c, err)
	}

	return c.Status(fiber.StatusOK).JSON(newPlansEnvelope(plans))
}

// newSubscriptionView maps the entity to its read model. now is the use case's
// clock (handlerUC.Now) — the trial fields are computed against it, never against
// a freshly-read time.Now() at the edge.
func newSubscriptionView(sub *Subscription, now time.Time) subscriptionView {
	view := subscriptionView{
		Plan:               sub.Plan,
		Status:             sub.Status,
		CurrentPeriodEnd:   sub.CurrentPeriodEnd,
		ActiveProcessLimit: sub.ActiveProcessLimit,
		LocalPlanID:        sub.PlanID,
		TrialEndsAt:        sub.TrialEndsAt,
		TrialStatus:        trialStatusNotInTrial,
	}

	if sub.Status != StatusTrialing || sub.TrialEndsAt == nil {
		return view
	}

	daysLeft := daysUntil(*sub.TrialEndsAt, now)
	view.DaysUntilTrialEnd = &daysLeft

	switch {
	case daysLeft < 0:
		view.TrialStatus = trialStatusExpired
	case daysLeft <= trialEndingSoonLeadDays:
		view.TrialStatus = trialStatusEndingSoon
	default:
		view.TrialStatus = trialStatusActive
	}

	return view
}

// newPlansEnvelope maps the plans to the client-facing envelope. The data slice is
// always initialized so an empty catalog serializes as [] rather than null.
func newPlansEnvelope(plans []StripePlan) plansEnvelope {
	views := make([]planView, 0, len(plans))
	for _, p := range plans {
		views = append(views, planView{
			PriceID:            p.PriceID,
			Name:               p.Name,
			Amount:             p.Amount,
			Interval:           p.Interval,
			ActiveProcessLimit: p.ActiveProcessLimit,
		})
	}
	return plansEnvelope{Data: views}
}

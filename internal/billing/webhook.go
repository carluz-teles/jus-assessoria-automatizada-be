package billing

import (
	"github.com/gofiber/fiber/v2"

	"github.com/jusassessoria/platform/lib/httpx"
)

// stripeSignatureHeader is the header Stripe signs each webhook with; the gateway
// verifies it against the RAW body.
const stripeSignatureHeader = "Stripe-Signature"

// WebhookHandler is the public HTTP edge for Stripe webhooks. It owns nothing but
// the use case: it reads the raw body and signature, hands them to HandleWebhook,
// and maps the typed result to a status. All verification, dispatch and
// persistence live behind the use case (docs §4b: the handler is a thin border).
type WebhookHandler struct {
	uc *UseCase
}

// NewWebhookHandler wires the handler to the billing use case.
func NewWebhookHandler(uc *UseCase) *WebhookHandler {
	return &WebhookHandler{uc: uc}
}

// Register mounts billing's public HTTP routes on r — the slice owns its own
// routing, so the api composes by calling this and never names the path itself.
// The Stripe webhook authenticates via its own signature (verified inside the use
// case), so it needs no bearer token and hangs off the router root, not /v1.
func (h *WebhookHandler) Register(r fiber.Router) {
	r.Post("/webhooks/stripe", h.Handle)
}

// Handle verifies and projects a Stripe webhook. The signature is checked over the
// RAW body (c.Body(), never a re-serialized struct — Stripe signs the exact
// bytes). A verified, dispatched event acks with 200; any typed error rides its
// Kind to a status (an invalid signature → 400), and a non-2xx makes Stripe retry.
func (h *WebhookHandler) Handle(c *fiber.Ctx) error {
	if err := h.uc.HandleWebhook(c.UserContext(), c.Body(), c.Get(stripeSignatureHeader)); err != nil {
		return httpx.WriteError(c, err)
	}
	return c.SendStatus(fiber.StatusOK)
}

package notifications

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/gofiber/fiber/v2"
	svix "github.com/svix/svix-webhooks/go"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/httpx"
)

// Resend webhook event types this slice acts on. Resend signs its webhooks with
// svix (the same scheme as Clerk), so the same three svix-* headers verify them.
const (
	eventEmailBounced    = "email.bounced"
	eventEmailComplained = "email.complained"
)

// bounceReason / complaintReason are the fallback delivery.error texts when the
// provider payload carries no richer message — human-readable, not sentinels.
const (
	bounceReason    = "recipient's mail server permanently rejected the message"
	complaintReason = "recipient marked the message as spam"
)

// signatureVerifier verifies a webhook's signature over the RAW request body. It is
// the seam that keeps the handler testable: production injects the svix adapter, the
// handler test injects a fake that accepts or rejects on demand.
type signatureVerifier interface {
	Verify(payload []byte, headers http.Header) error
}

// deliveryMarker is the narrow use-case port the webhook drives: flip a delivery's
// status from a provider callback. Keeping it an interface lets the handler test run
// without a database.
type deliveryMarker interface {
	MarkDeliveryOutcome(ctx context.Context, providerMessageID string, status DeliveryStatus, reason string) error
}

// WebhookHandler is the public HTTP edge for Resend delivery webhooks (bounce and
// complaint). It owns nothing but the signature verifier and the use case: it
// verifies the svix signature, decodes the event, and delegates the status flip. All
// persistence lives behind the use case (docs §4b: the handler is a thin border).
type WebhookHandler struct {
	verifier signatureVerifier
	uc       deliveryMarker
}

// NewWebhookHandler wires the handler to its signature verifier (NewSvixVerifier over
// RESEND_WEBHOOK_SECRET, injected from config by the api boot) and the webhook use
// case.
func NewWebhookHandler(verifier signatureVerifier, uc deliveryMarker) *WebhookHandler {
	return &WebhookHandler{verifier: verifier, uc: uc}
}

// Register mounts notifications' public HTTP routes on r — the slice owns its own
// routing, so the api composes by calling this and never names the path itself. The
// Resend webhook authenticates via its svix signature, so it needs no bearer token
// and hangs off the router root rather than the /v1 group.
func (h *WebhookHandler) Register(r fiber.Router) {
	r.Post("/webhooks/resend", h.Handle)
}

// resendEvent is the envelope every Resend webhook shares: a type discriminator and
// the event data. Only the fields this slice acts on are modeled; email_id is the
// provider message id that correlates back to the delivery's provider_message_id.
type resendEvent struct {
	Type string     `json:"type"`
	Data resendData `json:"data"`
}

type resendData struct {
	EmailID string `json:"email_id"`
	Bounce  struct {
		Message string `json:"message"`
	} `json:"bounce"`
}

// Handle verifies the svix signature over the RAW body — always, since the webhook is
// a public entry point — then applies the decoded event and acknowledges with 200. A
// bad signature is a 401; a malformed body a 400. Unknown/unhandled event types are
// acknowledged and ignored so Resend stops retrying them.
func (h *WebhookHandler) Handle(c *fiber.Ctx) error {
	body := c.Body()
	if err := h.verifier.Verify(body, svixHeaders(c)); err != nil {
		return httpx.WriteError(c, err)
	}

	var ev resendEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return httpx.WriteError(c, apperr.NewInvalid("malformed webhook payload"))
	}

	if err := h.dispatch(c.UserContext(), ev); err != nil {
		return httpx.WriteError(c, err)
	}

	return c.SendStatus(fiber.StatusOK)
}

// dispatch routes a verified event to the use case. Only bounce and complaint change
// state; every other type (delivered, opened, …) is a no-op so Resend stops retrying.
func (h *WebhookHandler) dispatch(ctx context.Context, ev resendEvent) error {
	switch ev.Type {
	case eventEmailBounced:
		reason := ev.Data.Bounce.Message
		if reason == "" {
			reason = bounceReason
		}
		return h.uc.MarkDeliveryOutcome(ctx, ev.Data.EmailID, DeliveryBounced, reason)

	case eventEmailComplained:
		return h.uc.MarkDeliveryOutcome(ctx, ev.Data.EmailID, DeliveryComplained, complaintReason)

	default:
		return nil
	}
}

// svixVerifier is the production adapter over svix-webhooks. It builds the verifier
// per call from the stored secret (the same molde as identity's Clerk webhook), so a
// missing/misconfigured secret only fails when a webhook actually arrives, never at
// boot.
type svixVerifier struct {
	secret string
}

// NewSvixVerifier returns the svix-backed signature verifier over the shared secret
// (RESEND_WEBHOOK_SECRET). Resend uses svix to sign, so verification is identical to
// Clerk's.
func NewSvixVerifier(secret string) signatureVerifier {
	return svixVerifier{secret: secret}
}

// Verify checks the svix signature over payload. A misconfigured secret is infra
// (surfaced, not a client fault); a signature mismatch is an Unauthorized the handler
// maps to 401.
func (v svixVerifier) Verify(payload []byte, headers http.Header) error {
	wh, err := svix.NewWebhook(v.secret)
	if err != nil {
		return apperr.NewInfra("resend webhook secret misconfigured", err)
	}
	if err := wh.Verify(payload, headers); err != nil {
		return apperr.NewUnauthorized("invalid signature")
	}
	return nil
}

// svixHeaders lifts the three svix signature headers Verify reads off the request.
// Building an http.Header explicitly (rather than copying every header) keeps the
// dependency on the transport minimal and the canonicalization predictable — the
// same shape identity uses for the Clerk webhook.
func svixHeaders(c *fiber.Ctx) http.Header {
	h := http.Header{}
	h.Set("svix-id", c.Get("svix-id"))
	h.Set("svix-timestamp", c.Get("svix-timestamp"))
	h.Set("svix-signature", c.Get("svix-signature"))
	return h
}

package identity

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gofiber/fiber/v2"
	svix "github.com/svix/svix-webhooks/go"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/httpx"
)

// Clerk webhook event types this slice provisions from (docs §4d.3, via 2).
const (
	eventOrganizationCreated = "organization.created"
	eventMembershipCreated   = "organizationMembership.created"
	eventUserUpdated         = "user.updated"
)

// WebhookHandler provisions the local projection of Clerk Organizations and Users
// from svix-signed webhooks (docs §4d.3). It owns nothing but the signing secret
// and the identity use cases; the write path (tx + idempotent upsert) lives in
// the UseCase.
type WebhookHandler struct {
	secret string
	uc     *UseCase
}

// NewWebhookHandler wires the handler to its signing secret (CLERK_WEBHOOK_SECRET,
// injected from config by the api boot slice) and the identity use cases.
func NewWebhookHandler(secret string, uc *UseCase) *WebhookHandler {
	return &WebhookHandler{secret: secret, uc: uc}
}

// Register mounts identity's public HTTP routes on r — the slice owns its own
// routing, so the api composes by calling this and never names the path itself.
// The Clerk provisioning webhook is the whole public surface for now: it
// authenticates via its svix signature (§4d.3), so it needs no bearer token and
// hangs off the router root rather than the /v1 group. When identity gains
// authenticated routes, add a RegisterV1(r fiber.Router) for the /v1 group
// alongside this one.
func (h *WebhookHandler) Register(r fiber.Router) {
	r.Post("/webhooks/clerk", h.Handle)
}

// clerkEvent is the envelope every Clerk webhook shares: a type discriminator and
// an opaque data object decoded per event in dispatch.
type clerkEvent struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

type orgData struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type membershipData struct {
	ID           string `json:"id"` // clerk_membership_id — the bridge to this membership
	Role         string `json:"role"`
	Organization struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"organization"`
	PublicUserData struct {
		UserID     string `json:"user_id"`
		Identifier string `json:"identifier"`
		FirstName  string `json:"first_name"`
		LastName   string `json:"last_name"`
	} `json:"public_user_data"`
}

type userData struct {
	ID                    string `json:"id"`
	FirstName             string `json:"first_name"`
	LastName              string `json:"last_name"`
	PrimaryEmailAddressID string `json:"primary_email_address_id"`
	EmailAddresses        []struct {
		ID           string `json:"id"`
		EmailAddress string `json:"email_address"`
	} `json:"email_addresses"`
	PrimaryPhoneNumberID string `json:"primary_phone_number_id"`
	PhoneNumbers         []struct {
		ID          string `json:"id"`
		PhoneNumber string `json:"phone_number"`
	} `json:"phone_numbers"`
	// unsafe_metadata.phone is where the onboarding wizard stores the phone: a
	// verified Clerk phone number needs an SMS round-trip, so the FE keeps the
	// (optional) phone in unsafe metadata. A verified phone_number still wins.
	UnsafeMetadata struct {
		Phone string `json:"phone"`
	} `json:"unsafe_metadata"`
}

// Handle verifies the svix signature — always, since the webhook is a public
// entry point — then provisions from the decoded event and acknowledges with 200.
// A bad signature is a 401; a malformed body a 400; a provisioning failure carries
// the use case's typed error (a not-yet-provisioned tenant, say, surfaces so Clerk
// retries). Unknown event types are acknowledged and ignored.
func (h *WebhookHandler) Handle(c *fiber.Ctx) error {
	wh, err := svix.NewWebhook(h.secret)
	if err != nil {
		return httpx.WriteError(c, apperr.NewInfra("webhook secret misconfigured", err))
	}

	body := c.Body()
	if err := wh.Verify(body, svixHeaders(c)); err != nil {
		return httpx.WriteError(c, apperr.NewUnauthorized("invalid signature"))
	}

	var ev clerkEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return httpx.WriteError(c, apperr.NewInvalid("malformed webhook payload"))
	}

	if err := h.dispatch(c.UserContext(), ev); err != nil {
		return httpx.WriteError(c, err)
	}

	return c.SendStatus(fiber.StatusOK)
}

// dispatch routes a verified event to the matching use case. Each branch decodes
// only the data shape it needs; an unmodeled type is a no-op so Clerk stops
// retrying it.
func (h *WebhookHandler) dispatch(ctx context.Context, ev clerkEvent) error {
	switch ev.Type {
	case eventOrganizationCreated:
		var d orgData
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			return apperr.NewInvalid("malformed organization payload")
		}
		_, err := h.uc.ProvisionTenant(ctx, d.ID, d.Name)
		return err

	case eventMembershipCreated:
		var d membershipData
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			return apperr.NewInvalid("malformed membership payload")
		}
		_, err := h.uc.OnMembershipCreated(
			ctx,
			d.PublicUserData.UserID,
			d.Organization.ID,
			d.ID,
			d.PublicUserData.Identifier,
			fullName(d.PublicUserData.FirstName, d.PublicUserData.LastName),
			mapClerkRole(d.Role),
		)
		return err

	case eventUserUpdated:
		var d userData
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			return apperr.NewInvalid("malformed user payload")
		}
		_, err := h.uc.SyncUser(ctx, d.ID, primaryEmail(d), fullName(d.FirstName, d.LastName), primaryPhone(d))
		return err

	default:
		return nil
	}
}

// svixHeaders lifts the three svix signature headers Verify reads off the request.
// Building an http.Header explicitly (rather than copying every header) keeps the
// dependency on the transport minimal and the canonicalization predictable.
func svixHeaders(c *fiber.Ctx) http.Header {
	h := http.Header{}
	h.Set("svix-id", c.Get("svix-id"))
	h.Set("svix-timestamp", c.Get("svix-timestamp"))
	h.Set("svix-signature", c.Get("svix-signature"))
	return h
}

// mapClerkRole maps a Clerk organization role to the product role. Clerk's admin
// role (org:admin, or the legacy "admin") becomes ADMIN; every other membership —
// org:member and friends — is a LAWYER. An unknown or empty role is a LAWYER by
// design, so a membership never silently gains admin rights.
func mapClerkRole(clerkRole string) Role {
	switch clerkRole {
	case "org:admin", "admin":
		return RoleAdmin
	default:
		return RoleLawyer
	}
}

// primaryEmail returns the user's primary email address, falling back to the
// first listed address when the primary id is unset or unmatched.
func primaryEmail(d userData) string {
	for _, e := range d.EmailAddresses {
		if e.ID == d.PrimaryEmailAddressID {
			return e.EmailAddress
		}
	}
	if len(d.EmailAddresses) > 0 {
		return d.EmailAddresses[0].EmailAddress
	}
	return ""
}

// primaryPhone returns the user's primary phone number, falling back to the
// first listed number when the primary id is unset or unmatched. An empty string
// (no phone on the Clerk User) means "leave the stored phone untouched" — the
// upsert COALESCEs it. Mirrors primaryEmail.
func primaryPhone(d userData) string {
	for _, p := range d.PhoneNumbers {
		if p.ID == d.PrimaryPhoneNumberID {
			return p.PhoneNumber
		}
	}
	if len(d.PhoneNumbers) > 0 {
		return d.PhoneNumbers[0].PhoneNumber
	}
	// Fallback: the phone the onboarding wizard set in unsafe metadata.
	return d.UnsafeMetadata.Phone
}

// fullName joins a Clerk first/last name into the single name app_user stores,
// collapsing the gap when either half is empty.
func fullName(first, last string) string {
	return strings.TrimSpace(first + " " + last)
}

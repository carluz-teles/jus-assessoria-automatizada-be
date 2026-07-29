package acquisition

import (
	"context"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/httpx"
	"github.com/jusassessoria/platform/lib/httpx/middleware"
)

// roleAdmin is the product role allowed to activate integrations. It is the
// wire-level string the auth middleware puts on the principal (identity.Role
// widened to a string at the edge); activation is an onboarding action, so only
// ADMIN may call it — a LAWYER gets 403.
const roleAdmin = "ADMIN"

// handlerUC is the narrow port the Handler uses from the acquisition use case —
// exactly the two methods the slice exposes.
type handlerUC interface {
	ActivateIntegration(ctx context.Context, tenantID string, sources []string, scope Scope) ([]*Integration, error)
	ListIntegrations(ctx context.Context, tenantID string) ([]*Integration, error)
}

// Handler is the acquisition HTTP surface. It owns its routing; the api only
// composes by calling RegisterV1.
type Handler struct {
	uc handlerUC
}

// NewHandler wires the handler to the acquisition use case.
func NewHandler(uc handlerUC) *Handler {
	return &Handler{uc: uc}
}

// RegisterV1 mounts acquisition's authenticated routes on the /v1 group. The
// write route is guarded by RequireRole(ADMIN); the read route is open to any
// authenticated principal of the tenant.
func (h *Handler) RegisterV1(r fiber.Router) {
	r.Post("/acquisition/integrations", middleware.RequireRole(roleAdmin), h.activate)
	r.Get("/acquisition/integrations", h.list)
}

// integrationView is the read model returned to the client — a per-endpoint DTO.
// credential_ref is deliberately absent: it is a server-side secret pointer.
type integrationView struct {
	ID        string    `json:"id"`
	Source    string    `json:"source"`
	Scope     Scope     `json:"scope"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// listEnvelope is the {data:[...]} response shape shared by both endpoints. The
// integration set per tenant is tiny (one row per source), so it is returned
// whole — no cursor pagination.
type listEnvelope struct {
	Data []integrationView `json:"data"`
}

// activate handles POST /v1/acquisition/integrations: validates the body,
// activates every requested source under the shared scope (rows + events in one
// tx), and returns 201 with the activated integrations. tenant_id comes from the
// verified principal, never the body.
func (h *Handler) activate(c *fiber.Ctx) error {
	var req ActivateIntegrationRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteError(c, apperr.NewInvalid("malformed request body"))
	}
	if err := req.Validate(); err != nil {
		return httpx.WriteValidationError(c, err)
	}

	tenantID := httpx.TenantFromCtx(c)
	integrations, err := h.uc.ActivateIntegration(c.UserContext(), tenantID, req.Sources, req.Scope)
	if err != nil {
		return httpx.WriteError(c, err)
	}

	return c.Status(fiber.StatusCreated).JSON(newListEnvelope(integrations))
}

// list handles GET /v1/acquisition/integrations: returns the tenant's
// integrations. tenant_id comes from the principal, so a caller only ever sees
// its own escritório's rows.
func (h *Handler) list(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	integrations, err := h.uc.ListIntegrations(c.UserContext(), tenantID)
	if err != nil {
		return httpx.WriteError(c, err)
	}

	return c.Status(fiber.StatusOK).JSON(newListEnvelope(integrations))
}

// newListEnvelope maps entities to the client-facing envelope. The data slice is
// always initialized so an empty result serializes as [] rather than null.
func newListEnvelope(integrations []*Integration) listEnvelope {
	views := make([]integrationView, 0, len(integrations))
	for _, integ := range integrations {
		views = append(views, integrationView{
			ID:        integ.ID,
			Source:    integ.Source,
			Scope:     integ.Scope,
			Status:    integ.Status,
			CreatedAt: integ.CreatedAt,
			UpdatedAt: integ.UpdatedAt,
		})
	}
	return listEnvelope{Data: views}
}

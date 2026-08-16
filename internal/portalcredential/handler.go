package portalcredential

import (
	"context"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/httpx"
)

// handlerUC is the narrow port the Handler uses from the use case.
type handlerUC interface {
	Configure(ctx context.Context, tenantID, appUserID, login, password string) (*PortalCredential, error)
	Get(ctx context.Context, tenantID, appUserID string) (*PortalCredential, error)
	Delete(ctx context.Context, tenantID, appUserID string) error
}

// Handler is the portalcredential HTTP surface. It owns its routing; the api
// only composes by calling RegisterV1.
type Handler struct {
	uc handlerUC
}

// NewHandler wires the handler to the write/read use case.
func NewHandler(uc handlerUC) *Handler {
	return &Handler{uc: uc}
}

// RegisterV1 mounts the slice's /v1 routes. All three act on the caller's OWN
// credential (tenant_id + app_user_id from the principal) — there is no :id in
// the path, and no route lets one advogado touch another's row.
func (h *Handler) RegisterV1(r fiber.Router) {
	r.Put("/scraping/portal-credential", h.configure)
	r.Get("/scraping/portal-credential", h.get)
	r.Delete("/scraping/portal-credential", h.delete)
}

// portalCredentialView is the read model returned to the client. Password and
// credential_ref are deliberately absent — the former never leaves the server
// in any form, the latter is a server-side secret-vault pointer.
type portalCredentialView struct {
	Login          string     `json:"login"`
	Status         string     `json:"status"`
	LastError      string     `json:"last_error,omitempty"`
	LastVerifiedAt *time.Time `json:"last_verified_at,omitempty"`
}

func newPortalCredentialView(c *PortalCredential) portalCredentialView {
	v := portalCredentialView{Login: c.Login, Status: c.Status, LastError: c.LastError}
	if !c.LastVerifiedAt.IsZero() {
		v.LastVerifiedAt = &c.LastVerifiedAt
	}
	return v
}

// configure handles PUT /v1/scraping/portal-credential: validates the body,
// tests the credential synchronously against the real TJSP eproc portal, and
// persists the outcome (see domain.go's Configure for the three-way branch).
// An explicit rejection (wrong login/password) is the only outcome that
// answers a client error (400); a successful OR inconclusive test both answer
// 200 with the saved credential's current state — the advogado's input is
// saved either way, per the ERD's "degradação honesta, dado sempre presente".
func (h *Handler) configure(c *fiber.Ctx) error {
	var req ConfigurePortalCredentialRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteError(c, apperr.NewInvalid("malformed request body"))
	}
	if err := req.Validate(); err != nil {
		return httpx.WriteValidationError(c, err)
	}

	principal, ok := httpx.PrincipalFromCtx(c)
	if !ok {
		return httpx.WriteError(c, apperr.NewUnauthorized("não autenticado"))
	}

	cred, err := h.uc.Configure(c.UserContext(), principal.TenantID, principal.UserID, req.Login, req.Password)
	if err != nil {
		return httpx.WriteError(c, err)
	}

	return c.Status(fiber.StatusOK).JSON(newPortalCredentialView(cred))
}

// get handles GET /v1/scraping/portal-credential: the caller's own credential
// state. Absent (never configured) is a typed 404.
func (h *Handler) get(c *fiber.Ctx) error {
	principal, ok := httpx.PrincipalFromCtx(c)
	if !ok {
		return httpx.WriteError(c, apperr.NewUnauthorized("não autenticado"))
	}

	cred, err := h.uc.Get(c.UserContext(), principal.TenantID, principal.UserID)
	if err != nil {
		return httpx.WriteError(c, err)
	}

	return c.Status(fiber.StatusOK).JSON(newPortalCredentialView(cred))
}

// delete handles DELETE /v1/scraping/portal-credential: removes the caller's
// own credential and its sealed secret. Absent is a typed 404 (not a
// swallowed no-op) — the caller learns there was nothing to remove.
func (h *Handler) delete(c *fiber.Ctx) error {
	principal, ok := httpx.PrincipalFromCtx(c)
	if !ok {
		return httpx.WriteError(c, apperr.NewUnauthorized("não autenticado"))
	}

	if err := h.uc.Delete(c.UserContext(), principal.TenantID, principal.UserID); err != nil {
		return httpx.WriteError(c, err)
	}

	return c.SendStatus(fiber.StatusNoContent)
}

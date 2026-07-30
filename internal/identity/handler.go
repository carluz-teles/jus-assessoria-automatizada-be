package identity

import (
	"context"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/httpx"
	"github.com/jusassessoria/platform/lib/httpx/middleware"
)

// handlerUC is the narrow port the Handler uses from the identity use case —
// exactly the two onboarding methods the HTTP surface exposes.
type handlerUC interface {
	GetMe(ctx context.Context, clerkUserID string) (Me, error)
	UpdateOrgProfile(ctx context.Context, tenantID string, profile OrgProfile) (*Tenant, error)
}

// Handler is identity's authenticated HTTP surface (the onboarding endpoints). It
// owns its routing; the api only composes by calling RegisterMe / RegisterV1. The
// two live under different auth regimes, so they mount through two methods: /me
// runs under the tenant-less AuthUser (no escritório yet), the profile write under
// the tenant-strict Auth.
type Handler struct {
	uc handlerUC
}

// NewHandler wires the handler to the identity use case.
func NewHandler(uc handlerUC) *Handler {
	return &Handler{uc: uc}
}

// RegisterMe mounts GET /identity/me. It belongs to the AuthUser subtree (verified
// user, no tenant required) so a freshly-signed-up user mid-onboarding can read
// their state; the api routes this path through AuthUser in its /v1 dispatch.
func (h *Handler) RegisterMe(r fiber.Router) {
	r.Get("/identity/me", h.me)
}

// RegisterV1 mounts the tenant-authenticated onboarding routes on the /v1 group.
// The profile write is guarded by RequireRole(ADMIN): completing the escritório's
// profile is an admin action, so a LAWYER gets 403.
func (h *Handler) RegisterV1(r fiber.Router) {
	r.Put("/organization/profile", middleware.RequireRole(string(RoleAdmin)), h.updateOrgProfile)
}

// meView is the read model returned by GET /identity/me. tenant_id and
// onboarding_completed_at are pointers so they serialize as JSON null when the
// caller has no tenant / has not completed onboarding.
type meView struct {
	UserID                string     `json:"user_id"`
	TenantID              *string    `json:"tenant_id"`
	OnboardingCompletedAt *time.Time `json:"onboarding_completed_at"`
}

// profileView is the saved company profile echoed back by PUT
// /organization/profile.
type profileView struct {
	CNPJ                  string     `json:"cnpj"`
	LegalName             string     `json:"legal_name"`
	TradeName             string     `json:"trade_name"`
	Address               *Address   `json:"address"`
	OnboardingCompletedAt *time.Time `json:"onboarding_completed_at"`
}

// me handles GET /v1/identity/me: it reads the Clerk user id AuthUser injected and
// returns the onboarding read model. Having no tenant is a valid 200 (nulls), not
// an error. A missing user id means AuthUser did not run — a 401, defensively.
func (h *Handler) me(c *fiber.Ctx) error {
	clerkUserID, ok := httpx.ClerkUserIDFromCtx(c)
	if !ok {
		return httpx.WriteError(c, apperr.NewUnauthorized("missing authenticated user"))
	}

	got, err := h.uc.GetMe(c.UserContext(), clerkUserID)
	if err != nil {
		return httpx.WriteError(c, err)
	}

	return c.Status(fiber.StatusOK).JSON(newMeView(got))
}

// updateOrgProfile handles PUT /v1/organization/profile: it validates the body,
// persists the profile onto the principal's tenant (row + event in one tx), and
// returns the saved profile. tenant_id comes from the verified principal, never
// the body.
func (h *Handler) updateOrgProfile(c *fiber.Ctx) error {
	var req UpdateOrgProfileRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteError(c, apperr.NewInvalid("malformed request body"))
	}
	if err := req.Validate(); err != nil {
		return httpx.WriteValidationError(c, err)
	}

	tenantID := httpx.TenantFromCtx(c)
	tenant, err := h.uc.UpdateOrgProfile(c.UserContext(), tenantID, req.toOrgProfile())
	if err != nil {
		return httpx.WriteError(c, err)
	}

	return c.Status(fiber.StatusOK).JSON(newProfileView(tenant))
}

// newMeView maps the Me read model to its client envelope.
func newMeView(me Me) meView {
	return meView{
		UserID:                me.UserID,
		TenantID:              me.TenantID,
		OnboardingCompletedAt: me.OnboardingCompletedAt,
	}
}

// newProfileView maps the saved tenant to the profile envelope.
func newProfileView(tenant *Tenant) profileView {
	return profileView{
		CNPJ:                  tenant.CNPJ,
		LegalName:             tenant.LegalName,
		TradeName:             tenant.TradeName,
		Address:               tenant.Address,
		OnboardingCompletedAt: tenant.OnboardingCompletedAt,
	}
}

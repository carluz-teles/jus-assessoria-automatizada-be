package onboarding

import (
	"context"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/httpx"
)

// handlerUC is the narrow port the Handler drives from the onboarding use
// case — exactly the two operations the HTTP surface exposes.
type handlerUC interface {
	GetProgress(ctx context.Context, tenantID, appUserID string) (Progress, error)
	Dismiss(ctx context.Context, tenantID, appUserID string) error
}

// Handler is onboarding's authenticated HTTP surface (the post-signup
// activation widget). It owns its routing; the api only composes by calling
// Register.
type Handler struct {
	uc handlerUC
}

// NewHandler wires the handler to the onboarding use case.
func NewHandler(uc handlerUC) *Handler {
	return &Handler{uc: uc}
}

// Register mounts onboarding's routes on the /v1 group. Both routes are open
// to ANY authenticated role (ADMIN and LAWYER read/dismiss alike) — the
// widget is not an admin-only surface, so neither route uses RequireRole.
func (h *Handler) Register(r fiber.Router) {
	r.Get("/onboarding/progress", h.getProgress)
	r.Patch("/onboarding/dismiss", h.dismiss)
}

// stepsView mirrors Steps for the JSON response.
type stepsView struct {
	SourcesConnected bool `json:"sources_connected"`
	MembersInvited   bool `json:"members_invited"`
	FirstTriagem     bool `json:"first_triagem"`
	FirstAnalise     bool `json:"first_analise"`
	FirstPeca        bool `json:"first_peca"`
}

// progressView is the response envelope for GET /v1/onboarding/progress.
// DismissedAt is a pointer so it serializes as JSON null until the caller
// dismisses the widget.
type progressView struct {
	Steps       stepsView  `json:"steps"`
	DismissedAt *time.Time `json:"dismissed_at"`
}

// getProgress handles GET /v1/onboarding/progress: the caller's tenant-wide
// activation checklist plus their own dismissal state. tenant_id and the
// caller's app_user id come from the verified principal, never the request —
// so a caller only ever sees its own tenant's progress and its own dismissal.
func (h *Handler) getProgress(c *fiber.Ctx) error {
	principal, ok := httpx.PrincipalFromCtx(c)
	if !ok {
		return httpx.WriteError(c, apperr.NewUnauthorized("missing authenticated principal"))
	}

	progress, err := h.uc.GetProgress(c.UserContext(), principal.TenantID, principal.UserID)
	if err != nil {
		return httpx.WriteError(c, err)
	}

	return c.Status(fiber.StatusOK).JSON(newProgressView(progress))
}

// dismiss handles PATCH /v1/onboarding/dismiss: no request body — it stamps
// the caller's own dismissal and always answers 204. tenant_id and the
// caller's app_user id come from the verified principal, never the request.
func (h *Handler) dismiss(c *fiber.Ctx) error {
	principal, ok := httpx.PrincipalFromCtx(c)
	if !ok {
		return httpx.WriteError(c, apperr.NewUnauthorized("missing authenticated principal"))
	}

	if err := h.uc.Dismiss(c.UserContext(), principal.TenantID, principal.UserID); err != nil {
		return httpx.WriteError(c, err)
	}

	return c.SendStatus(fiber.StatusNoContent)
}

// newProgressView maps the Progress read model to its client envelope.
func newProgressView(p Progress) progressView {
	return progressView{
		Steps: stepsView{
			SourcesConnected: p.Steps.SourcesConnected,
			MembersInvited:   p.Steps.MembersInvited,
			FirstTriagem:     p.Steps.FirstTriagem,
			FirstAnalise:     p.Steps.FirstAnalise,
			FirstPeca:        p.Steps.FirstPeca,
		},
		DismissedAt: p.DismissedAt,
	}
}

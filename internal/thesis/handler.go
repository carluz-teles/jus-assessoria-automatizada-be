package thesis

import (
	"github.com/gofiber/fiber/v2"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/httpx"
)

type Handler struct {
	uc *UseCase
}

func NewHandler(uc *UseCase) *Handler {
	return &Handler{uc: uc}
}

func (h *Handler) RegisterV1(r fiber.Router) {
	r.Post("/thesis", h.createThesis)
	r.Get("/thesis/:id", h.getThesis)
	r.Post("/thesis/:id/approve", h.approveThesis)
	r.Post("/thesis/:id/discard", h.discardThesis)
	r.Post("/thesis/:id/coverage", h.checkCoverage)

	r.Post("/thesis/segments", h.createSegment)
	r.Get("/pecas/:id/theses", h.listThesesByDraft)
	r.Get("/pecas/:id/segments", h.listSegmentsByDraft)
	r.Get("/pecas/:id/coverage", h.getCoverageSummary)
	r.Get("/pecas/:id/coverage/detail", h.listCoverageByDraft)
}

func (h *Handler) createThesis(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	var req CreateThesisRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteError(c, apperr.NewInvalid("invalid request body"))
	}
	if err := req.Validate(); err != nil {
		return httpx.WriteValidationError(c, err)
	}
	cmd := CreateThesisCommand{
		TenantID:        tenantID,
		DraftID:         req.DraftID,
		PieceProfileKey: req.PieceProfileKey,
		NotificationID:  req.NotificationID,
		Enunciado:       req.Enunciado,
		Forca:           req.Forca,
		Anchors:         req.Anchors,
	}
	t, err := h.uc.CreateThesis(c.UserContext(), cmd)
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"data": t})
}

func (h *Handler) getThesis(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	thesisID := c.Params("id")
	t, err := h.uc.GetThesisByID(c.UserContext(), tenantID, thesisID)
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.JSON(fiber.Map{"data": t})
}

func (h *Handler) approveThesis(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	thesisID := c.Params("id")
	t, err := h.uc.ApproveThesis(c.UserContext(), tenantID, thesisID)
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.JSON(fiber.Map{"data": t})
}

func (h *Handler) discardThesis(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	thesisID := c.Params("id")
	t, err := h.uc.DiscardThesis(c.UserContext(), tenantID, thesisID)
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.JSON(fiber.Map{"data": t})
}

func (h *Handler) checkCoverage(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	thesisID := c.Params("id")
	cov, err := h.uc.CheckCoverage(c.UserContext(), tenantID, thesisID)
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"data": cov})
}

func (h *Handler) createSegment(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	var req CreateSegmentRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteError(c, apperr.NewInvalid("invalid request body"))
	}
	if err := req.Validate(); err != nil {
		return httpx.WriteValidationError(c, err)
	}
	cmd := CreateSegmentCommand{
		TenantID:         tenantID,
		DraftID:          req.DraftID,
		ThesisID:         req.ThesisID,
		ProfileSectionID: req.ProfileSectionID,
		Conteudo:         req.Conteudo,
	}
	s, err := h.uc.CreateSegment(c.UserContext(), cmd)
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"data": s})
}

func (h *Handler) listThesesByDraft(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	draftID := c.Params("id")
	theses, err := h.uc.ListThesesByDraft(c.UserContext(), tenantID, draftID)
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.JSON(fiber.Map{"data": theses})
}

func (h *Handler) listSegmentsByDraft(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	draftID := c.Params("id")
	segments, err := h.uc.ListSegmentsByDraft(c.UserContext(), tenantID, draftID)
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.JSON(fiber.Map{"data": segments})
}

func (h *Handler) getCoverageSummary(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	draftID := c.Params("id")
	summary, err := h.uc.GetCoverageSummary(c.UserContext(), tenantID, draftID)
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.JSON(fiber.Map{"data": summary})
}

func (h *Handler) listCoverageByDraft(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	draftID := c.Params("id")
	coverage, err := h.uc.ListCoverageByDraft(c.UserContext(), tenantID, draftID)
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.JSON(fiber.Map{"data": coverage})
}

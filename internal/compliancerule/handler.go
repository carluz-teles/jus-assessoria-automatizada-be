package compliancerule

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
	r.Get("/compliance-rules", h.listRules)
	r.Get("/compliance-rules/:key", h.getRule)
	r.Post("/compliance-rules", h.createRule)
	r.Patch("/compliance-rules/:key", h.updateRule)
	r.Delete("/compliance-rules/:key", h.deleteRule)

	r.Get("/piece-profiles/:key/rules", h.listRulesByProfile)
	r.Post("/piece-profiles/:key/rules", h.addRuleToProfile)
	r.Delete("/piece-profiles/:key/rules/:ruleKey", h.removeRuleFromProfile)

	r.Get("/piece-profiles/:key/sections/:sectionId/rules", h.listRulesBySection)
	r.Post("/piece-profiles/:key/sections/:sectionId/rules", h.addRuleToSection)
	r.Delete("/piece-profiles/:key/sections/:sectionId/rules/:ruleKey", h.removeRuleFromSection)
}

func (h *Handler) listRules(c *fiber.Ctx) error {
	rules, err := h.uc.ListRules(c.UserContext())
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.JSON(fiber.Map{"data": rules})
}

func (h *Handler) getRule(c *fiber.Ctx) error {
	key := c.Params("key")
	rule, err := h.uc.GetRule(c.UserContext(), key)
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.JSON(fiber.Map{"data": rule})
}

func (h *Handler) createRule(c *fiber.Ctx) error {
	var req CreateRuleRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteError(c, apperr.NewInvalid("invalid request body"))
	}
	if err := req.Validate(); err != nil {
		return httpx.WriteValidationError(c, err)
	}
	cmd := CreateRuleCommand{
		Key:         req.Key,
		Descricao:   req.Descricao,
		Severidade:  req.Severidade,
		FonteLegal:  req.FonteLegal,
		Verificacao: req.Verificacao,
	}
	rule, err := h.uc.CreateRule(c.UserContext(), cmd)
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"data": rule})
}

func (h *Handler) updateRule(c *fiber.Ctx) error {
	key := c.Params("key")
	var req UpdateRuleRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteError(c, apperr.NewInvalid("invalid request body"))
	}
	if err := req.Validate(); err != nil {
		return httpx.WriteValidationError(c, err)
	}
	cmd := UpdateRuleCommand{Key: key}
	if req.Descricao != "" {
		cmd.Descricao = &req.Descricao
	}
	if req.Severidade != "" {
		cmd.Severidade = &req.Severidade
	}
	if req.FonteLegal != "" {
		cmd.FonteLegal = &req.FonteLegal
	}
	if req.Verificacao != "" {
		cmd.Verificacao = &req.Verificacao
	}
	rule, err := h.uc.UpdateRule(c.UserContext(), cmd)
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.JSON(fiber.Map{"data": rule})
}

func (h *Handler) deleteRule(c *fiber.Ctx) error {
	key := c.Params("key")
	if err := h.uc.DeleteRule(c.UserContext(), key); err != nil {
		return httpx.WriteError(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

func (h *Handler) listRulesByProfile(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	profileKey := c.Params("key")
	rules, err := h.uc.ListRulesByProfile(c.UserContext(), tenantID, profileKey)
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.JSON(fiber.Map{"data": rules})
}

func (h *Handler) addRuleToProfile(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	profileKey := c.Params("key")
	var req AddRuleToProfileRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteError(c, apperr.NewInvalid("invalid request body"))
	}
	if err := req.Validate(); err != nil {
		return httpx.WriteValidationError(c, err)
	}
	cmd := AddProfileRuleCommand{
		TenantID:           tenantID,
		PieceProfileKey:    profileKey,
		RuleKey:            req.RuleKey,
		OverrideSeveridade: &req.OverrideSeveridade,
	}
	rule, err := h.uc.AddRuleToProfile(c.UserContext(), cmd)
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"data": rule})
}

func (h *Handler) removeRuleFromProfile(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	profileKey := c.Params("key")
	ruleKey := c.Params("ruleKey")
	if err := h.uc.RemoveRuleFromProfile(c.UserContext(), tenantID, profileKey, ruleKey); err != nil {
		return httpx.WriteError(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

func (h *Handler) listRulesBySection(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	sectionID := c.Params("sectionId")
	rules, err := h.uc.ListRulesBySection(c.UserContext(), tenantID, sectionID)
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.JSON(fiber.Map{"data": rules})
}

func (h *Handler) addRuleToSection(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	sectionID := c.Params("sectionId")
	var req AddRuleToProfileRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteError(c, apperr.NewInvalid("invalid request body"))
	}
	if err := req.Validate(); err != nil {
		return httpx.WriteValidationError(c, err)
	}
	cmd := AddSectionRuleCommand{
		TenantID:         tenantID,
		ProfileSectionID: sectionID,
		RuleKey:          req.RuleKey,
	}
	rule, err := h.uc.AddRuleToSection(c.UserContext(), cmd)
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"data": rule})
}

func (h *Handler) removeRuleFromSection(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	sectionID := c.Params("sectionId")
	ruleKey := c.Params("ruleKey")
	if err := h.uc.RemoveRuleFromSection(c.UserContext(), tenantID, sectionID, ruleKey); err != nil {
		return httpx.WriteError(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

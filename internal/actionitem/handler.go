package actionitem

import (
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/httpx"
)

// Handler is the actionitem HTTP surface: the confirmar/descartar buttons on the
// "Analisar esta intimação" card (docs handoff §5). It owns its routing; the api only
// composes by calling RegisterV1.
type Handler struct {
	uc *UseCase
}

// NewHandler wires the handler to the use case.
func NewHandler(uc *UseCase) *Handler {
	return &Handler{uc: uc}
}

// RegisterV1 mounts actionitem's authenticated routes on the /v1 group.
func (h *Handler) RegisterV1(r fiber.Router) {
	r.Post("/action-items/:id/confirmar", h.confirmar)
	r.Post("/action-items/:id/descartar", h.descartar)
	r.Post("/action-items/:id/reclassificar", h.reclassificar)
}

// confirmar handles POST /v1/action-items/:id/confirmar: promotes an a_confirmar
// providência's tipo to confiável. tenant_id comes from the principal, never the body.
func (h *Handler) confirmar(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	item, err := h.uc.Confirmar(c.UserContext(), tenantID, c.Params("id"))
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(fiber.Map{"data": newActionItemResponse(item)})
}

// descartar handles POST /v1/action-items/:id/descartar: dismisses the providência.
// tenant_id comes from the principal, never the body.
func (h *Handler) descartar(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	item, err := h.uc.Descartar(c.UserContext(), tenantID, c.Params("id"))
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(fiber.Map{"data": newActionItemResponse(item)})
}

// reclassificar handles POST /v1/action-items/:id/reclassificar (fatia 5, docs §7 questão
// 4): overrides the providência's tipo/piece_profile_key. tenant_id comes from the
// principal, never the body; a malformed or out-of-catalog body is a 400.
func (h *Handler) reclassificar(c *fiber.Ctx) error {
	var req ReclassifyRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteError(c, apperr.NewInvalid("malformed request body"))
	}
	if err := req.Validate(); err != nil {
		return httpx.WriteValidationError(c, err)
	}

	tenantID := httpx.TenantFromCtx(c)
	item, err := h.uc.Reclassificar(c.UserContext(), tenantID, c.Params("id"), req.PieceProfileKey, req.Tipo)
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(fiber.Map{"data": newActionItemResponse(item)})
}

// actionItemResponse is the client-facing shape of an ActionItem: snake_case json tags,
// matching the convention every other slice's HTTP surface follows (e.g.
// internal/deadline/handler.go's confirmResponse, internal/pieceprofile/handler.go's
// profileResponse). entity.go's ActionItem has no json tags on purpose — it is the pure-Go
// aggregate, not a wire format — so handlers must never serialize it directly.
type actionItemResponse struct {
	ID              string     `json:"id"`
	TenantID        string     `json:"tenant_id"`
	IntimationID    string     `json:"intimation_id"`
	CourtRecordID   string     `json:"court_record_id,omitempty"`
	Tipo            string     `json:"tipo"`
	GeraPeca        bool       `json:"gera_peca"`
	PieceProfileKey string     `json:"piece_profile_key,omitempty"`
	TipoOrigem      TipoOrigem `json:"tipo_origem"`
	TipoStatus      TipoStatus `json:"tipo_status"`
	DeadlineID      string     `json:"deadline_id,omitempty"`
	Confianca       *float64   `json:"confianca,omitempty"`
	Status          Status     `json:"status"`
	TaskID          string     `json:"task_id,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

// newActionItemResponse maps the domain aggregate to its wire format.
func newActionItemResponse(a *ActionItem) actionItemResponse {
	return actionItemResponse{
		ID:              a.ID,
		TenantID:        a.TenantID,
		IntimationID:    a.IntimationID,
		CourtRecordID:   a.CourtRecordID,
		Tipo:            a.Tipo,
		GeraPeca:        a.GeraPeca,
		PieceProfileKey: a.PieceProfileKey,
		TipoOrigem:      a.TipoOrigem,
		TipoStatus:      a.TipoStatus,
		DeadlineID:      a.DeadlineID,
		Confianca:       a.Confianca,
		Status:          a.Status,
		TaskID:          a.TaskID,
		CreatedAt:       a.CreatedAt,
		UpdatedAt:       a.UpdatedAt,
	}
}

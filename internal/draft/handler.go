package draft

import (
	"context"
	"math"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/jusassessoria/platform/lib/httpx"
)

// handler.go is the draft slice's HTTP surface — POST /v1/pecas, GET /v1/pecas/:id,
// PATCH /v1/pecas/:id. The slice owns its routing; cmd/api only composes by calling
// RegisterV1. tenant_id ALWAYS comes from the verified principal, never the body or
// the path.

// writer is the narrow port the Handler uses from the write use case.
type writer interface {
	Create(ctx context.Context, cmd CreateCommand) (CreateResult, error)
	Patch(ctx context.Context, cmd PatchCommand) (*PatchResult, error)
	GetDetail(ctx context.Context, tenantID, draftID string) (*DraftDetailView, error)
}

// Handler is the draft HTTP surface. It owns its routing; cmd/api composes via
// RegisterV1.
type Handler struct {
	uc writer
}

// NewHandler wires the handler to the use case.
func NewHandler(uc writer) *Handler {
	return &Handler{uc: uc}
}

// RegisterV1 mounts the peças routes on the /v1 group.
func (h *Handler) RegisterV1(r fiber.Router) {
	r.Post("/pecas", h.createPeca)
	r.Get("/pecas/:id", h.getPeca)
	r.Patch("/pecas/:id", h.patchPeca)
}

// ─── POST /v1/pecas ──────────────────────────────────────────────────────────

// createPeca handles POST /v1/pecas. Returns 201 on first creation, 200 on
// idempotent (same tenant+intimation_id already has a draft).
func (h *Handler) createPeca(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)

	var req CreateRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteError(c, err)
	}
	if err := req.Validate(); err != nil {
		return httpx.WriteValidationError(c, err)
	}

	result, err := h.uc.Create(c.UserContext(), CreateCommand{
		TenantID:     tenantID,
		Source:       req.Source,
		IntimationID: req.IntimationID,
		CaseID:       req.CaseID,
		PieceType:    req.PieceType,
		Title:        req.Title,
	})
	if err != nil {
		return httpx.WriteError(c, err)
	}

	status := fiber.StatusCreated
	if !result.IsNewDraft {
		status = fiber.StatusOK
	}
	return c.Status(status).JSON(fiber.Map{"data": draftToResponse(result.Draft)})
}

// ─── GET /v1/pecas/:id ────────────────────────────────────────────────────────

// getPeca handles GET /v1/pecas/:id.
func (h *Handler) getPeca(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	draftID := c.Params("id")

	view, err := h.uc.GetDetail(c.UserContext(), tenantID, draftID)
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.JSON(fiber.Map{"data": detailToResponse(view)})
}

// ─── PATCH /v1/pecas/:id ─────────────────────────────────────────────────────

// patchPeca handles PATCH /v1/pecas/:id (autosave).
func (h *Handler) patchPeca(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	draftID := c.Params("id")

	var req PatchRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteError(c, err)
	}
	if err := req.Validate(); err != nil {
		return httpx.WriteValidationError(c, err)
	}

	result, err := h.uc.Patch(c.UserContext(), PatchCommand{
		TenantID: tenantID,
		DraftID:  draftID,
		Content:  req.Content,
		Title:    req.Title,
	})
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.JSON(fiber.Map{"data": patchToResponse(result)})
}

// ─── Response shapes ─────────────────────────────────────────────────────────

// draftResponse is the POST /v1/pecas response body — shared between the 201 and
// the 200 idempotent path.
type draftResponse struct {
	ID           string  `json:"id"`
	TenantID     string  `json:"tenant_id"`
	CaseID       string  `json:"case_id,omitempty"`
	IntimationID string  `json:"intimation_id,omitempty"`
	PieceType    string  `json:"piece_type"`
	Title        string  `json:"title"`
	Content      *string `json:"content"`
	Status       string  `json:"status"`
	SagaState    string  `json:"saga_state"`
	CreatedAt    string  `json:"created_at"`
	UpdatedAt    string  `json:"updated_at"`
}

func draftToResponse(d *Draft) draftResponse {
	var content *string
	if d.Content != "" {
		c := d.Content
		content = &c
	}
	return draftResponse{
		ID:           d.ID,
		TenantID:     d.TenantID,
		CaseID:       d.CaseID,
		IntimationID: d.IntimationID,
		PieceType:    d.PieceType,
		Title:        d.Title,
		Content:      content,
		Status:       d.Status,
		SagaState:    d.SagaState,
		CreatedAt:    d.CreatedAt.Format(time.RFC3339),
		UpdatedAt:    d.UpdatedAt.Format(time.RFC3339),
	}
}

// patchResponse is the PATCH /v1/pecas/:id response body.
type patchResponse struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	UpdatedAt string `json:"updated_at"`
}

func patchToResponse(r *PatchResult) patchResponse {
	return patchResponse{
		ID:        r.ID,
		Title:     r.Title,
		UpdatedAt: r.UpdatedAt.Format(time.RFC3339),
	}
}

// detailResponse is the GET /v1/pecas/:id response body.
type detailResponse struct {
	ID        string `json:"id"`
	PieceType string `json:"piece_type"`
	Title     string `json:"title"`
	Content   string `json:"content"`
	Status    string `json:"status"`
	SagaState string `json:"saga_state"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`

	Intimation *intimationResponse `json:"intimation,omitempty"`
	Process    *processResponse    `json:"process,omitempty"`
	Deadline   *deadlineResponse   `json:"deadline,omitempty"`
}

type intimationResponse struct {
	ID              string `json:"id"`
	Type            string `json:"type"`
	Content         string `json:"content"`
	MadeAvailableAt string `json:"made_available_at"`
	DeadlineStartAt string `json:"deadline_start_at"`
}

type processResponse struct {
	CaseID        string `json:"case_id"`
	CourtRecordID string `json:"court_record_id"`
	CNJNumber     string `json:"cnj_number"`
	Court         string `json:"court"`
	Degree        string `json:"degree"`
	Class         string `json:"class"`
	Subject       string `json:"subject"`
	JudgingBody   string `json:"judging_body"`
}

type deadlineResponse struct {
	ID       string `json:"id"`
	EndDate  string `json:"end_date"`
	DaysLeft int    `json:"days_left"`
	Status   string `json:"status"`
}

func detailToResponse(v *DraftDetailView) detailResponse {
	resp := detailResponse{
		ID:        v.ID,
		PieceType: v.PieceType,
		Title:     v.Title,
		Content:   v.Content,
		Status:    v.Status,
		SagaState: v.SagaState,
		CreatedAt: v.CreatedAt.Format(time.RFC3339),
		UpdatedAt: v.UpdatedAt.Format(time.RFC3339),
	}

	if v.Intimation != nil {
		resp.Intimation = &intimationResponse{
			ID:              v.Intimation.ID,
			Type:            v.Intimation.Type,
			Content:         v.Intimation.Content,
			MadeAvailableAt: v.Intimation.MadeAvailableAt.Format(time.DateOnly),
			DeadlineStartAt: v.Intimation.DeadlineStartAt.Format(time.DateOnly),
		}
	}

	if v.Process != nil {
		resp.Process = &processResponse{
			CaseID:        v.Process.CaseID,
			CourtRecordID: v.Process.CourtRecordID,
			CNJNumber:     v.Process.CNJNumber,
			Court:         v.Process.Court,
			Degree:        v.Process.Degree,
			Class:         v.Process.Class,
			Subject:       v.Process.Subject,
			JudgingBody:   v.Process.JudgingBody,
		}
	}

	if v.Deadline != nil {
		daysLeft := daysLeftFromNow(v.Deadline.EndDate)
		resp.Deadline = &deadlineResponse{
			ID:       v.Deadline.ID,
			EndDate:  v.Deadline.EndDate.Format(time.DateOnly),
			DaysLeft: daysLeft,
			Status:   v.Deadline.Status,
		}
	}

	return resp
}

// daysLeftFromNow computes calendar days remaining until endDate (from today). A
// past date yields 0. This is a simple calendar subtraction — the exact business-day
// countdown lives in lib/calendar and is not needed at read time.
func daysLeftFromNow(endDate time.Time) int {
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	end := time.Date(endDate.Year(), endDate.Month(), endDate.Day(), 0, 0, 0, 0, time.UTC)
	diff := end.Sub(today)
	days := int(math.Ceil(diff.Hours() / 24))
	if days < 0 {
		return 0
	}
	return days
}

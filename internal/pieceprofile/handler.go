package pieceprofile

import (
	"context"

	"github.com/gofiber/fiber/v2"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/httpx"
)

type profileReader interface {
	GetProfile(ctx context.Context, tenantID, key string) (*PieceProfile, error)
	ListProfiles(ctx context.Context, tenantID, matterKey string) ([]PieceProfile, error)
	ListRequirements(ctx context.Context, tenantID, profileKey string) ([]ProfileRequirement, error)
	GetMatter(ctx context.Context, key string) (*Matter, error)
	ListMatters(ctx context.Context) ([]Matter, error)
	GetBaseSkeleton(ctx context.Context, key string) (*BaseSkeleton, error)
	ListBaseSkeletons(ctx context.Context) ([]BaseSkeleton, error)
	GetFormatProfile(ctx context.Context, key string) (*FormatProfile, error)
	ListFormatProfiles(ctx context.Context) ([]FormatProfile, error)
}

type profileWriter interface {
	CreateProfile(ctx context.Context, cmd CreateProfileCommand) (*PieceProfile, error)
	UpdateProfile(ctx context.Context, cmd UpdateProfileCommand) (*PieceProfile, error)
	CreateSection(ctx context.Context, cmd CreateSectionCommand) (*ProfileSection, error)
	UpdateSection(ctx context.Context, cmd UpdateSectionCommand) (*ProfileSection, error)
	DeleteSection(ctx context.Context, tenantID, sectionID string) error
	CreateVersion(ctx context.Context, cmd CreateVersionCommand) (*PieceProfileVersion, error)
	GetVersion(ctx context.Context, tenantID, profileKey, version string) (*PieceProfileVersion, error)
}

type Handler struct {
	reader profileReader
	writer profileWriter
}

func NewHandler(reader profileReader, writer profileWriter) *Handler {
	return &Handler{reader: reader, writer: writer}
}

// RegisterV1 mounts every route this slice owns. matter/base_skeleton/format_profile/
// profile_requirement are v1 read-only (cadastro de dados — seed direto, sem rota de
// escrita via API, docs/erd-tipos-de-peca.md §7.4).
func (h *Handler) RegisterV1(r fiber.Router) {
	r.Get("/piece-profiles", h.listProfiles)
	r.Get("/piece-profiles/:key", h.getProfile)
	r.Post("/piece-profiles", h.createProfile)
	r.Patch("/piece-profiles/:key", h.updateProfile)
	r.Get("/piece-profiles/:key/requirements", h.listRequirements)
	r.Post("/piece-profiles/:key/sections", h.createSection)
	r.Patch("/piece-profiles/:key/sections/:sectionId", h.updateSection)
	r.Delete("/piece-profiles/:key/sections/:sectionId", h.deleteSection)
	r.Post("/piece-profiles/:key/versions", h.createVersion)
	r.Get("/piece-profiles/:key/versions/:version", h.getVersion)

	r.Get("/matters", h.listMatters)
	r.Get("/matters/:key", h.getMatter)
	r.Get("/base-skeletons", h.listBaseSkeletons)
	r.Get("/base-skeletons/:key", h.getBaseSkeleton)
	r.Get("/format-profiles", h.listFormatProfiles)
	r.Get("/format-profiles/:key", h.getFormatProfile)
}

func (h *Handler) listProfiles(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	matterKey := c.Query("matter_key")

	profiles, err := h.reader.ListProfiles(c.UserContext(), tenantID, matterKey)
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.JSON(fiber.Map{"data": profilesToResponse(profiles)})
}

func (h *Handler) getProfile(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	key := c.Params("key")

	p, err := h.reader.GetProfile(c.UserContext(), tenantID, key)
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.JSON(fiber.Map{"data": profileToResponse(p)})
}

func (h *Handler) createProfile(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)

	var req CreateProfileRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteError(c, apperr.NewInvalid("corpo inválido"))
	}
	if err := req.Validate(); err != nil {
		return httpx.WriteValidationError(c, err)
	}

	p, err := h.writer.CreateProfile(c.UserContext(), CreateProfileCommand{
		TenantID:         tenantID,
		Key:              req.Key,
		Nome:             req.Nome,
		Polo:             req.Polo,
		MatterKey:        req.MatterKey,
		BaseSkeletonKey:  req.BaseSkeletonKey,
		FormatProfileKey: req.FormatProfileKey,
		FonteLegal:       req.FonteLegal,
	})
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"data": profileToResponse(p)})
}

func (h *Handler) updateProfile(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	key := c.Params("key")

	var req UpdateProfileRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteError(c, apperr.NewInvalid("corpo inválido"))
	}
	if err := req.Validate(); err != nil {
		return httpx.WriteValidationError(c, err)
	}

	p, err := h.writer.UpdateProfile(c.UserContext(), UpdateProfileCommand{
		TenantID:         tenantID,
		Key:              key,
		Nome:             req.Nome,
		Polo:             req.Polo,
		MatterKey:        req.MatterKey,
		BaseSkeletonKey:  req.BaseSkeletonKey,
		FormatProfileKey: req.FormatProfileKey,
		FonteLegal:       req.FonteLegal,
	})
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.JSON(fiber.Map{"data": profileToResponse(p)})
}

func (h *Handler) listRequirements(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	profileKey := c.Params("key")

	reqs, err := h.reader.ListRequirements(c.UserContext(), tenantID, profileKey)
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.JSON(fiber.Map{"data": requirementsToResponse(reqs)})
}

func (h *Handler) createSection(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	profileKey := c.Params("key")

	var req CreateSectionRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteError(c, apperr.NewInvalid("corpo inválido"))
	}
	if err := req.Validate(); err != nil {
		return httpx.WriteValidationError(c, err)
	}

	s, err := h.writer.CreateSection(c.UserContext(), CreateSectionCommand{
		TenantID:    tenantID,
		ProfileKey:  profileKey,
		Key:         req.Key,
		Titulo:      req.Titulo,
		Ordem:       req.Ordem,
		Obrigatoria: req.Obrigatoria,
		Origem:      req.Origem,
		AceitaTeses: req.AceitaTeses,
		FonteLegal:  req.FonteLegal,
	})
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"data": sectionToResponse(s)})
}

func (h *Handler) updateSection(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	sectionID := c.Params("sectionId")

	var req UpdateSectionRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteError(c, apperr.NewInvalid("corpo inválido"))
	}
	if err := req.Validate(); err != nil {
		return httpx.WriteValidationError(c, err)
	}

	s, err := h.writer.UpdateSection(c.UserContext(), UpdateSectionCommand{
		TenantID:    tenantID,
		SectionID:   sectionID,
		Key:         req.Key,
		Titulo:      req.Titulo,
		Ordem:       req.Ordem,
		Obrigatoria: req.Obrigatoria,
		Origem:      req.Origem,
		AceitaTeses: req.AceitaTeses,
		FonteLegal:  req.FonteLegal,
	})
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.JSON(fiber.Map{"data": sectionToResponse(s)})
}

func (h *Handler) deleteSection(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	sectionID := c.Params("sectionId")

	if err := h.writer.DeleteSection(c.UserContext(), tenantID, sectionID); err != nil {
		return httpx.WriteError(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

func (h *Handler) createVersion(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	profileKey := c.Params("key")

	var req CreateVersionRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteError(c, apperr.NewInvalid("corpo inválido"))
	}
	if err := req.Validate(); err != nil {
		return httpx.WriteValidationError(c, err)
	}

	v, err := h.writer.CreateVersion(c.UserContext(), CreateVersionCommand{
		TenantID:   tenantID,
		ProfileKey: profileKey,
		Version:    req.Version,
	})
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"data": versionToResponse(v)})
}

func (h *Handler) getVersion(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	profileKey := c.Params("key")
	version := c.Params("version")

	v, err := h.writer.GetVersion(c.UserContext(), tenantID, profileKey, version)
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.JSON(fiber.Map{"data": versionToResponse(v)})
}

func (h *Handler) listMatters(c *fiber.Ctx) error {
	matters, err := h.reader.ListMatters(c.UserContext())
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.JSON(fiber.Map{"data": matters})
}

func (h *Handler) getMatter(c *fiber.Ctx) error {
	m, err := h.reader.GetMatter(c.UserContext(), c.Params("key"))
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.JSON(fiber.Map{"data": m})
}

func (h *Handler) listBaseSkeletons(c *fiber.Ctx) error {
	skeletons, err := h.reader.ListBaseSkeletons(c.UserContext())
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.JSON(fiber.Map{"data": skeletons})
}

func (h *Handler) getBaseSkeleton(c *fiber.Ctx) error {
	bs, err := h.reader.GetBaseSkeleton(c.UserContext(), c.Params("key"))
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.JSON(fiber.Map{"data": bs})
}

func (h *Handler) listFormatProfiles(c *fiber.Ctx) error {
	profiles, err := h.reader.ListFormatProfiles(c.UserContext())
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.JSON(fiber.Map{"data": profiles})
}

func (h *Handler) getFormatProfile(c *fiber.Ctx) error {
	fp, err := h.reader.GetFormatProfile(c.UserContext(), c.Params("key"))
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.JSON(fiber.Map{"data": fp})
}

type profileResponse struct {
	Key              string                `json:"key"`
	Nome             string                `json:"nome"`
	Polo             string                `json:"polo"`
	MatterKey        string                `json:"matter_key"`
	BaseSkeletonKey  string                `json:"base_skeleton_key"`
	FormatProfileKey string                `json:"format_profile_key,omitempty"`
	VersionAtual     string                `json:"version_atual"`
	FonteLegal       []byte                `json:"fonte_legal,omitempty"`
	CreatedAt        string                `json:"created_at"`
	UpdatedAt        string                `json:"updated_at"`
	Sections         []sectionResponse     `json:"sections"`
	Requirements     []requirementResponse `json:"requirements"`
}

type sectionResponse struct {
	ID          string `json:"id"`
	Key         string `json:"key"`
	Titulo      string `json:"titulo"`
	Ordem       int    `json:"ordem"`
	Obrigatoria string `json:"obrigatoria"`
	Origem      string `json:"origem"`
	AceitaTeses bool   `json:"aceita_teses"`
	FonteLegal  []byte `json:"fonte_legal,omitempty"`
}

type requirementResponse struct {
	ID          string `json:"id"`
	Campo       string `json:"campo"`
	Obrigatorio bool   `json:"obrigatorio"`
	FonteLegal  []byte `json:"fonte_legal,omitempty"`
}

type versionResponse struct {
	ID           string `json:"id"`
	Version      string `json:"version"`
	VigenteDesde string `json:"vigente_desde"`
	Snapshot     []byte `json:"snapshot"`
}

func profileToResponse(p *PieceProfile) profileResponse {
	sections := make([]sectionResponse, 0, len(p.Sections))
	for i := range p.Sections {
		sections = append(sections, sectionToResponse(&p.Sections[i]))
	}
	requirements := make([]requirementResponse, 0, len(p.Requirements))
	for i := range p.Requirements {
		requirements = append(requirements, requirementToResponse(&p.Requirements[i]))
	}
	return profileResponse{
		Key:              p.Key,
		Nome:             p.Nome,
		Polo:             p.Polo,
		MatterKey:        p.MatterKey,
		BaseSkeletonKey:  p.BaseSkeletonKey,
		FormatProfileKey: p.FormatProfileKey,
		VersionAtual:     p.VersionAtual,
		FonteLegal:       p.FonteLegal,
		CreatedAt:        p.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		UpdatedAt:        p.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
		Sections:         sections,
		Requirements:     requirements,
	}
}

func profilesToResponse(profiles []PieceProfile) []profileResponse {
	out := make([]profileResponse, 0, len(profiles))
	for i := range profiles {
		out = append(out, profileToResponse(&profiles[i]))
	}
	return out
}

func sectionToResponse(s *ProfileSection) sectionResponse {
	return sectionResponse{
		ID:          s.ID,
		Key:         s.Key,
		Titulo:      s.Titulo,
		Ordem:       s.Ordem,
		Obrigatoria: s.Obrigatoria,
		Origem:      s.Origem,
		AceitaTeses: s.AceitaTeses,
		FonteLegal:  s.FonteLegal,
	}
}

func requirementToResponse(r *ProfileRequirement) requirementResponse {
	return requirementResponse{
		ID:          r.ID,
		Campo:       r.Campo,
		Obrigatorio: r.Obrigatorio,
		FonteLegal:  r.FonteLegal,
	}
}

func requirementsToResponse(reqs []ProfileRequirement) []requirementResponse {
	out := make([]requirementResponse, 0, len(reqs))
	for i := range reqs {
		out = append(out, requirementToResponse(&reqs[i]))
	}
	return out
}

func versionToResponse(v *PieceProfileVersion) versionResponse {
	return versionResponse{
		ID:           v.ID,
		Version:      v.Version,
		VigenteDesde: v.VigenteDesde.Format("2006-01-02T15:04:05Z07:00"),
		Snapshot:     v.Snapshot,
	}
}

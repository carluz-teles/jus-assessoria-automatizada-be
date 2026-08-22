package certificate

import (
	"context"
	"io"

	"github.com/gofiber/fiber/v2"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/httpx"
)

// handler.go is the certificate slice's HTTP surface (the A1 milestone): the
// secure upload (POST /v1/certificates, multipart), the list (GET /v1/certificates,
// metadata only), and the revoke (DELETE /v1/certificates/:id). The slice owns its
// routing; cmd/api only composes by calling Register. tenant_id AND owner_user_id
// come from the verified principal, never the body/path.
//
// SECURITY: the password arrives in a multipart field, is handed straight to the
// use case, and is never logged. The list/create responses are CertificateView —
// metadata only, no key material.

// maxUploadBytes caps the .pfx upload. An A1 file is a few KB; 1 MB is generous and
// bounds the bytes we buffer + encrypt in memory.
const maxUploadBytes = int64(1 << 20)

// uc is the narrow port the Handler uses from the use case — exactly the endpoint
// methods.
type uc interface {
	Create(ctx context.Context, cmd CreateCommand) (CertificateView, error)
	Preview(ctx context.Context, cmd PreviewCommand) (PreviewResult, error)
	List(ctx context.Context, tenantID string) ([]CertificateView, error)
	Revoke(ctx context.Context, tenantID, id string) error
}

// Handler is the certificate HTTP surface. It owns its routing; the api only
// composes by calling Register.
type Handler struct {
	uc uc
}

// NewHandler wires the handler to the use case (injected as a narrow port so tests
// substitute a fake).
func NewHandler(uc uc) *Handler {
	return &Handler{uc: uc}
}

// Register mounts the certificate routes on the /v1 group. The api only composes.
// The static /certificates/preview is declared before /certificates/:id so Fiber
// never captures "preview" as an :id (it is only on the DELETE verb, but keeping
// the ordering explicit is defensive).
func (h *Handler) Register(r fiber.Router) {
	r.Post("/certificates/preview", h.preview)
	r.Post("/certificates", h.create)
	r.Get("/certificates", h.list)
	r.Delete("/certificates/:id", h.revoke)
}

// readPFXUpload reads and bounds the multipart {file, password} both create and
// preview consume. It returns the raw .pfx bytes + the password, or a typed 4xx.
// The password is returned to the caller and NEVER logged here.
func readPFXUpload(c *fiber.Ctx) (pfxData []byte, password string, err error) {
	password = c.FormValue("password")
	if password == "" {
		return nil, "", ErrPasswordRequired
	}

	fileHeader, err := c.FormFile("file")
	if err != nil || fileHeader == nil {
		return nil, "", ErrEmptyFile
	}
	if fileHeader.Size == 0 {
		return nil, "", ErrEmptyFile
	}
	if fileHeader.Size > maxUploadBytes {
		return nil, "", apperr.NewInvalid("certificate file is too large")
	}

	f, err := fileHeader.Open()
	if err != nil {
		return nil, "", apperr.NewInvalid("could not read the uploaded file")
	}
	defer f.Close()

	// Bound the read to maxUploadBytes+1 so a lying Content-Length cannot make us
	// buffer an unbounded body.
	pfxData, err = io.ReadAll(io.LimitReader(f, maxUploadBytes+1))
	if err != nil {
		return nil, "", apperr.NewInvalid("could not read the uploaded file")
	}
	if int64(len(pfxData)) > maxUploadBytes {
		return nil, "", apperr.NewInvalid("certificate file is too large")
	}
	if len(pfxData) == 0 {
		return nil, "", ErrEmptyFile
	}
	return pfxData, password, nil
}

// create handles POST /v1/certificates: multipart {file, password}. It reads the
// file part (≤ maxUploadBytes) and the password, then parses + envelope-encrypts +
// stores. tenant_id + owner_user_id come from the verified principal. A wrong
// password / expired / malformed file is a typed 4xx via the {kind,message,details}
// envelope; success is 201 + CertificateView.
func (h *Handler) create(c *fiber.Ctx) error {
	principal, ok := httpx.PrincipalFromCtx(c)
	if !ok {
		return httpx.WriteError(c, apperr.NewUnauthorized("missing principal"))
	}

	pfxData, password, err := readPFXUpload(c)
	if err != nil {
		return httpx.WriteError(c, err)
	}

	view, err := h.uc.Create(c.UserContext(), CreateCommand{
		TenantID:    principal.TenantID,
		OwnerUserID: principal.UserID,
		PFXData:     pfxData,
		Password:    password,
	})
	if err != nil {
		return httpx.WriteError(c, err)
	}

	return c.Status(fiber.StatusCreated).JSON(view)
}

// preview handles POST /v1/certificates/preview: multipart {file, password}. It
// parses + validates the .pfx and returns the metadata + checks, storing NOTHING
// (the wizard's read-only "Validação" step). A wrong password / malformed file is a
// typed 4xx; a successful parse is 200 + PreviewResult even when a check is false.
// tenant scoping is unnecessary (nothing is persisted), but the route is still
// behind the authenticated /v1 group.
func (h *Handler) preview(c *fiber.Ctx) error {
	pfxData, password, err := readPFXUpload(c)
	if err != nil {
		return httpx.WriteError(c, err)
	}

	result, err := h.uc.Preview(c.UserContext(), PreviewCommand{
		PFXData:  pfxData,
		Password: password,
	})
	if err != nil {
		return httpx.WriteError(c, err)
	}

	return c.Status(fiber.StatusOK).JSON(result)
}

// certificatesEnvelope is the {data:[...]} response for the list. The set per
// tenant is small (a handful of lawyers), so it is returned whole — no cursor.
type certificatesEnvelope struct {
	Data []CertificateView `json:"data"`
}

// list handles GET /v1/certificates: the tenant's certificates (metadata only),
// wrapped in {data:[...]}. tenant_id comes from the principal.
func (h *Handler) list(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	views, err := h.uc.List(c.UserContext(), tenantID)
	if err != nil {
		return httpx.WriteError(c, err)
	}
	if views == nil {
		views = []CertificateView{}
	}
	return c.Status(fiber.StatusOK).JSON(certificatesEnvelope{Data: views})
}

// revoke handles DELETE /v1/certificates/:id: soft-revoke. tenant_id comes from
// the principal, the id from the path. A miss/foreign/already-revoked id is
// ErrCertificateNotFound → 404; success is 204 No Content.
func (h *Handler) revoke(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	if err := h.uc.Revoke(c.UserContext(), tenantID, c.Params("id")); err != nil {
		return httpx.WriteError(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

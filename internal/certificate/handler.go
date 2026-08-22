package certificate

import (
	"context"
	"encoding/base64"
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
	Sign(ctx context.Context, cmd SignCommand) (SignResult, error)
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
	r.Post("/certificates/:id/sign", h.sign)
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

// signRequest is the POST /v1/certificates/:id/sign body. Password is the session
// password used ONLY to decrypt the .pfx server-side (never persisted, never
// logged); DigestSHA256 is the base64 of the raw 32-byte SHA-256 the caller already
// computed over its document. The certificate id comes from the path, tenant +
// signer from the principal — never the body.
type signRequest struct {
	Password     string `json:"password"`
	DigestSHA256 string `json:"digest_sha256"`
}

// Validate enforces the shape before the use case runs (ozzo-style, but the body is
// tiny so an explicit check is clearer than a rule set): a password is required and
// the digest must be present. The 32-byte length is enforced after base64 decoding.
func (r signRequest) Validate() error {
	if r.Password == "" {
		return ErrPasswordRequired
	}
	if r.DigestSHA256 == "" {
		return ErrInvalidDigest
	}
	return nil
}

// signResponse is the sign result: the signature and the DER cert chain (leaf
// first), both base64. No key material — the signature is public by nature.
type signResponse struct {
	Signature string   `json:"signature"`
	CertChain []string `json:"cert_chain"`
}

// sign handles POST /v1/certificates/:id/sign: JSON {password, digest_sha256}. It
// decodes the digest, then decrypts the stored .pfx with the session password and
// signs the digest with the certificate's RSA private key server-side. tenant +
// signer come from the principal, the certificate id from the path. A wrong
// password / bad digest → typed 4xx; a missing cert → 404; success → 200 +
// {signature, cert_chain} (base64). The password is never logged.
func (h *Handler) sign(c *fiber.Ctx) error {
	principal, ok := httpx.PrincipalFromCtx(c)
	if !ok {
		return httpx.WriteError(c, apperr.NewUnauthorized("missing principal"))
	}

	var req signRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteError(c, apperr.NewInvalid("invalid request body"))
	}
	if err := req.Validate(); err != nil {
		return httpx.WriteError(c, err)
	}

	digest, err := base64.StdEncoding.DecodeString(req.DigestSHA256)
	if err != nil {
		return httpx.WriteError(c, ErrInvalidDigest)
	}

	res, err := h.uc.Sign(c.UserContext(), SignCommand{
		TenantID:      principal.TenantID,
		SignerUserID:  principal.UserID,
		CertificateID: c.Params("id"),
		Password:      req.Password,
		Digest:        digest,
	})
	if err != nil {
		return httpx.WriteError(c, err)
	}

	chain := make([]string, 0, len(res.ChainDER))
	for _, der := range res.ChainDER {
		chain = append(chain, base64.StdEncoding.EncodeToString(der))
	}
	return c.Status(fiber.StatusOK).JSON(signResponse{
		Signature: base64.StdEncoding.EncodeToString(res.Signature),
		CertChain: chain,
	})
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

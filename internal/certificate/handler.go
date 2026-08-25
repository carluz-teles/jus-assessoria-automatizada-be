package certificate

import (
	"encoding/base64"
	"io"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/httpx"
)

// Limite duro no upload: um .pfx sério raramente passa 100KB. 2MB é margem
// generosa; acima disso é abuso/erro do cliente.
const maxPFXSize = 2 * 1024 * 1024

// Handler expõe as 5 rotas do slice. O owner de todo cert cadastrado é o
// user_id do principal — ninguém cadastra em nome de outro.
type Handler struct {
	uc *UseCase
}

// New constrói o handler. Recebe o use case pronto (com storage + cipher).
func New(uc *UseCase) *Handler {
	return &Handler{uc: uc}
}

// RegisterV1 monta as rotas sob /v1. Chame de cmd/api/main.go.
func (h *Handler) RegisterV1(r fiber.Router) {
	r.Post("/certificates", h.upload)
	r.Post("/certificates/preview", h.preview)
	r.Get("/certificates", h.list)
	r.Delete("/certificates/:id", h.revoke)
	r.Post("/certificates/:id/sign", h.sign)
}

// preview: POST /v1/certificates/preview (multipart: file, password) →
// CertificadoPreviewResult (bate com o FE 1:1). Nada persistido.
func (h *Handler) preview(c *fiber.Ctx) error {
	pfx, password, err := readMultipartPFX(c)
	if err != nil {
		return httpx.WriteError(c, err)
	}
	res, err := h.uc.Preview(c.UserContext(), pfx, password)
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(previewToResponse(res))
}

// upload: POST /v1/certificates (multipart: file, password) → CertificateView.
// A senha é usada só pra abrir o PKCS#12 e é DESCARTADA (não persiste).
func (h *Handler) upload(c *fiber.Ctx) error {
	pfx, password, err := readMultipartPFX(c)
	if err != nil {
		return httpx.WriteError(c, err)
	}
	tenantID := httpx.TenantFromCtx(c)
	p, ok := httpx.PrincipalFromCtx(c)
	if !ok {
		return httpx.WriteError(c, apperr.NewUnauthorized("missing principal"))
	}
	cert, err := h.uc.Upload(c.UserContext(), tenantID, p.UserID, pfx, password)
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(certificateToView(cert, ""))
}

// list: GET /v1/certificates → { data: CertificateView[] }.
func (h *Handler) list(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	items, err := h.uc.List(c.UserContext(), tenantID)
	if err != nil {
		return httpx.WriteError(c, err)
	}
	views := make([]certificateView, 0, len(items))
	for i := range items {
		views = append(views, certificateToView(&items[i].Certificate, items[i].OwnerUserName))
	}
	return c.Status(fiber.StatusOK).JSON(fiber.Map{"data": views})
}

// revoke: DELETE /v1/certificates/:id → 204. Soft-delete.
func (h *Handler) revoke(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	id := c.Params("id")
	if err := h.uc.Revoke(c.UserContext(), tenantID, id); err != nil {
		return httpx.WriteError(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// sign: POST /v1/certificates/:id/sign body { password, digest_sha256 } →
// { signature (base64), cert_chain (base64[]) }. Senha da sessão, nunca
// persistida.
func (h *Handler) sign(c *fiber.Ctx) error {
	var req SignRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteError(c, apperr.NewInvalid("malformed request body"))
	}
	if err := req.Validate(); err != nil {
		return httpx.WriteValidationError(c, err)
	}
	digest, err := base64.StdEncoding.DecodeString(req.DigestSHA256B64)
	if err != nil || len(digest) != 32 {
		return httpx.WriteError(c, apperr.NewInvalid("digest_sha256 deve ser SHA-256 base64 (32 bytes)"))
	}
	tenantID := httpx.TenantFromCtx(c)
	id := c.Params("id")
	res, err := h.uc.Sign(c.UserContext(), tenantID, id, req.Password, digest)
	if err != nil {
		return httpx.WriteError(c, err)
	}
	chainB64 := make([]string, 0, len(res.Chain))
	for _, der := range res.Chain {
		chainB64 = append(chainB64, base64.StdEncoding.EncodeToString(der))
	}
	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"signature":  base64.StdEncoding.EncodeToString(res.Signature),
		"cert_chain": chainB64,
	})
}

// ── helpers ─────────────────────────────────────────────────────────────────

// readMultipartPFX lê o campo `file` + `password` de um multipart. Valida
// tamanho e forma. Devolve apperr tipado em caso de erro de borda.
func readMultipartPFX(c *fiber.Ctx) ([]byte, string, error) {
	fileHdr, err := c.FormFile("file")
	if err != nil {
		return nil, "", apperr.NewInvalid("campo 'file' ausente ou inválido")
	}
	if fileHdr.Size > maxPFXSize {
		return nil, "", apperr.NewInvalid("arquivo maior que o limite permitido")
	}
	f, err := fileHdr.Open()
	if err != nil {
		return nil, "", apperr.NewInvalid("não foi possível abrir o arquivo")
	}
	defer f.Close()
	pfx, err := io.ReadAll(f)
	if err != nil {
		return nil, "", apperr.NewInvalid("erro lendo o arquivo")
	}
	password := c.FormValue("password")
	if password == "" {
		return nil, "", apperr.NewInvalid("campo 'password' ausente")
	}
	return pfx, password, nil
}

// ── response DTOs (bate com o FE certificado.ts) ─────────────────────────────

type certificateView struct {
	ID            string  `json:"id"`
	SubjectCN     string  `json:"subject_cn"`
	OAB           string  `json:"oab"`
	Issuer        string  `json:"issuer"`
	Serial        string  `json:"serial"`
	NotBefore     string  `json:"not_before"`
	NotAfter      string  `json:"not_after"`
	Fingerprint   string  `json:"fingerprint"`
	OwnerUserID   string  `json:"owner_user_id"`
	OwnerUserName string  `json:"owner_user_name,omitempty"`
	CreatedAt     string  `json:"created_at"`
	RevokedAt     *string `json:"revoked_at"`
}

func certificateToView(c *Certificate, ownerName string) certificateView {
	var revoked *string
	if c.RevokedAt != nil {
		s := c.RevokedAt.Format(time.RFC3339)
		revoked = &s
	}
	return certificateView{
		ID:            c.ID,
		SubjectCN:     c.SubjectCN,
		OAB:           c.OAB,
		Issuer:        c.Issuer,
		Serial:        c.Serial,
		NotBefore:     c.NotBefore.Format(time.RFC3339),
		NotAfter:      c.NotAfter.Format(time.RFC3339),
		Fingerprint:   c.Fingerprint,
		OwnerUserID:   c.OwnerUserID,
		OwnerUserName: ownerName,
		CreatedAt:     c.CreatedAt.Format(time.RFC3339),
		RevokedAt:     revoked,
	}
}

type previewChecksResponse struct {
	NaoExpirado    bool `json:"nao_expirado"`
	CadeiaOk       bool `json:"cadeia_ok"`
	TitularConfere bool `json:"titular_confere"`
}

type previewResponse struct {
	SubjectCN   string                `json:"subject_cn"`
	OAB         string                `json:"oab"`
	Issuer      string                `json:"issuer"`
	Serial      string                `json:"serial"`
	NotBefore   string                `json:"not_before"`
	NotAfter    string                `json:"not_after"`
	Fingerprint string                `json:"fingerprint"`
	Checks      previewChecksResponse `json:"checks"`
}

func previewToResponse(p *PreviewResult) previewResponse {
	return previewResponse{
		SubjectCN:   p.Meta.SubjectCN,
		OAB:         p.Meta.OAB,
		Issuer:      p.Meta.Issuer,
		Serial:      p.Meta.Serial,
		NotBefore:   p.Meta.NotBefore.Format(time.RFC3339),
		NotAfter:    p.Meta.NotAfter.Format(time.RFC3339),
		Fingerprint: p.Meta.Fingerprint,
		Checks: previewChecksResponse{
			NaoExpirado:    p.Checks.NaoExpirado,
			CadeiaOk:       p.Checks.CadeiaOk,
			TitularConfere: p.Checks.TitularConfere,
		},
	}
}

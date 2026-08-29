package court

import (
	"io"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/httpx"
	"github.com/jusassessoria/platform/lib/totp"
)

// maxQRScreenshotSize bounds the "print" upload — a phone/browser screenshot of a
// QR code is a few hundred KB at most; 5MB is generous margin, not a real limit.
const maxQRScreenshotSize = 5 * 1024 * 1024

// Handler exposes the "Conexões com tribunais" screen's two endpoints. Every route
// resolves tenant/user from the verified principal — never the body.
type Handler struct {
	uc *UseCase
}

// New builds the handler around a ready UseCase (providers already registered).
func New(uc *UseCase) *Handler {
	return &Handler{uc: uc}
}

// RegisterV1 mounts the slice's routes under /v1. Called once from cmd/api/main.go.
func (h *Handler) RegisterV1(r fiber.Router) {
	r.Post("/court-connections", h.create)
	r.Get("/court-connections", h.list)
	r.Post("/court-connections/:id/connect", h.connect)
	r.Post("/court-connections/:id/mfa-seed", h.submitMFASeed)
}

type createConnectionRequest struct {
	Court                string `json:"court"`
	System               string `json:"system"`
	AuthenticationMethod string `json:"authentication_method"`
	CredentialRef        string `json:"credential_ref"`
	CertificateRef       string `json:"certificate_ref"`
}

// create: POST /v1/court-connections → registers the connection (DISCONNECTED).
// The FE follows up with POST .../connect to actually authenticate — kept as two
// steps so a slow first Connect never blocks the create response.
func (h *Handler) create(c *fiber.Ctx) error {
	var req createConnectionRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.WriteError(c, apperr.NewInvalid("corpo da requisição inválido"))
	}
	tenantID := httpx.TenantFromCtx(c)
	p, ok := httpx.PrincipalFromCtx(c)
	if !ok {
		return httpx.WriteError(c, apperr.NewUnauthorized("missing principal"))
	}
	conn, err := h.uc.CreateConnection(c.UserContext(), tenantID, p.UserID, req.Court, req.System,
		AuthenticationMethod(req.AuthenticationMethod), req.CredentialRef, req.CertificateRef)
	if err != nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(connectionToView(conn))
}

// list: GET /v1/court-connections → { data: connectionView[] }.
func (h *Handler) list(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	conns, err := h.uc.ListConnections(c.UserContext(), tenantID)
	if err != nil {
		return httpx.WriteError(c, err)
	}
	views := make([]connectionView, 0, len(conns))
	for i := range conns {
		views = append(views, connectionToView(&conns[i]))
	}
	return c.Status(fiber.StatusOK).JSON(fiber.Map{"data": views})
}

// connect: POST /v1/court-connections/:id/connect → authenticates now (may run
// automated MFA enrollment inline — see UseCase.Connect's doc) and returns the
// resulting state. The FE also gets court.connection_state_changed if it's
// listening; this response is the synchronous confirmation for the button that
// triggered it.
func (h *Handler) connect(c *fiber.Ctx) error {
	tenantID := httpx.TenantFromCtx(c)
	id := c.Params("id")
	conn, err := h.uc.Connect(c.UserContext(), tenantID, id)
	if err != nil && conn == nil {
		return httpx.WriteError(c, err)
	}
	// Connect returns BOTH the (possibly failed) connection state AND the error —
	// the FE wants to see the resulting status even when Connect itself failed
	// (e.g. MFA_ENROLLMENT_REQUIRED is not a 500, it's informative state).
	return c.Status(fiber.StatusOK).JSON(connectionToView(conn))
}

// submitMFASeed: POST /v1/court-connections/:id/mfa-seed (multipart) — the
// human-assisted, ONE-TIME capture UseCase.SubmitMFASeed's doc describes. Accepts
// EITHER a "qr" file field (a screenshot of the enrollment/reconfiguration QR code)
// OR a "secret" text field (the manual-entry key Keycloak's "Unable to scan?" toggle
// shows, when the lawyer can copy it directly instead of a screenshot) — whichever
// the portal happened to show. Exactly one is required; both together is also fine
// (the file wins, since a screenshot is unambiguous where a hand-copied string could
// have a typo).
func (h *Handler) submitMFASeed(c *fiber.Ctx) error {
	secret, err := extractMFASecret(c)
	if err != nil {
		return httpx.WriteError(c, err)
	}
	tenantID := httpx.TenantFromCtx(c)
	id := c.Params("id")
	conn, err := h.uc.SubmitMFASeed(c.UserContext(), tenantID, id, secret)
	if err != nil && conn == nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(connectionToView(conn))
}

// extractMFASecret resolves the request into a raw TOTP secret ready for
// totp.GenerateCode — decoding a QR screenshot when "qr" is present, otherwise
// parsing "secret" as either a bare manual-entry key or a full otpauth:// URI (see
// totp.ExtractSecret's doc for why both shapes are accepted).
func extractMFASecret(c *fiber.Ctx) (string, error) {
	if fileHdr, err := c.FormFile("qr"); err == nil {
		if fileHdr.Size > maxQRScreenshotSize {
			return "", apperr.NewInvalid("imagem maior que o limite permitido")
		}
		f, err := fileHdr.Open()
		if err != nil {
			return "", apperr.NewInvalid("não foi possível abrir a imagem")
		}
		defer f.Close()
		imgBytes, err := io.ReadAll(f)
		if err != nil {
			return "", apperr.NewInvalid("erro lendo a imagem")
		}
		text, err := totp.DecodeQRImage(imgBytes)
		if err != nil {
			return "", apperr.NewInvalid("não foi possível ler o QR code na imagem enviada")
		}
		secret, err := totp.ExtractSecret(text)
		if err != nil {
			return "", apperr.NewInvalid("o QR code não contém uma chave TOTP reconhecível")
		}
		return secret, nil
	}

	raw := c.FormValue("secret")
	if raw == "" {
		return "", apperr.NewInvalid("envie o campo 'qr' (print) ou 'secret' (texto)")
	}
	secret, err := totp.ExtractSecret(raw)
	if err != nil {
		return "", apperr.NewInvalid("texto enviado não contém uma chave TOTP reconhecível")
	}
	return secret, nil
}

// connectionView is the wire shape — never includes a *_ref (those are internal
// vault pointers, meaningless and unsafe to expose to the FE).
type connectionView struct {
	ID                   string  `json:"id"`
	Court                string  `json:"court"`
	System               string  `json:"system"`
	AuthenticationMethod string  `json:"authentication_method"`
	Status               string  `json:"status"`
	LastAuthenticatedAt  *string `json:"last_authenticated_at,omitempty"`
	Error                string  `json:"error,omitempty"`
	CreatedAt            string  `json:"created_at"`
}

func connectionToView(c *CourtConnection) connectionView {
	v := connectionView{
		ID:                   c.ID,
		Court:                c.Court,
		System:               c.System,
		AuthenticationMethod: string(c.AuthenticationMethod),
		Status:               string(c.Status),
		Error:                c.Error,
		CreatedAt:            c.CreatedAt.Format(time.RFC3339),
	}
	if c.LastAuthenticatedAt != nil {
		s := c.LastAuthenticatedAt.Format(time.RFC3339)
		v.LastAuthenticatedAt = &s
	}
	return v
}

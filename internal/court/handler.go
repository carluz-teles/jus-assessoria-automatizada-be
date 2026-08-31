package court

import (
	"fmt"
	"io"
	"strconv"
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
// EITHER a "qr" file field OR a "secret" text field (the file wins when both are
// present — unambiguous where a hand-copied string could have a typo). Three
// shapes are recognized inside either field (see totp.ExtractAccounts's doc):
// eproc's own enrollment QR/manual key (always exactly one candidate — the
// original flow, unchanged), or a Google Authenticator "Export accounts"
// migration QR, which can bundle SEVERAL accounts (the lawyer's whole
// authenticator app, not just eproc's).
//
// When extraction yields more than one candidate, this does NOT guess — it
// responds 200 with needsSelectionView (labels only, never secrets) so the FE
// can show a picker and resubmit the SAME qr/secret plus the chosen
// "account_index". Stateless on purpose: no server-side cache of candidates:
// the QR is already on the lawyer's device, resubmitting it is a normal upload
// UX, and it means a decoded secret is never round-tripped to the client
// unconfirmed.
func (h *Handler) submitMFASeed(c *fiber.Ctx) error {
	candidates, err := extractMFACandidates(c)
	if err != nil {
		return httpx.WriteError(c, err)
	}

	var secret string
	switch len(candidates) {
	case 0:
		// extractMFACandidates never actually returns this (every path either
		// errors or yields ≥1) — guarded explicitly rather than relying on that
		// invariant silently holding forever.
		return httpx.WriteError(c, apperr.NewInvalid("nenhuma conta TOTP encontrada"))
	case 1:
		secret = candidates[0].Secret
	default:
		idx, ok, err := selectedAccountIndex(c, len(candidates))
		if err != nil {
			return httpx.WriteError(c, err)
		}
		if !ok {
			return c.Status(fiber.StatusOK).JSON(newNeedsSelectionView(candidates))
		}
		secret = candidates[idx].Secret
	}

	tenantID := httpx.TenantFromCtx(c)
	id := c.Params("id")
	conn, err := h.uc.SubmitMFASeed(c.UserContext(), tenantID, id, secret)
	if err != nil && conn == nil {
		return httpx.WriteError(c, err)
	}
	return c.Status(fiber.StatusOK).JSON(connectionToView(conn))
}

// extractMFACandidates resolves the request into every TOTP account
// totp.ExtractAccounts found — decoding a QR screenshot when "qr" is present,
// otherwise parsing "secret" as text. Almost always returns exactly one
// candidate; more than one only when a Google Authenticator migration QR
// bundled several accounts (see submitMFASeed's doc).
func extractMFACandidates(c *fiber.Ctx) ([]totp.Account, error) {
	if fileHdr, err := c.FormFile("qr"); err == nil {
		if fileHdr.Size > maxQRScreenshotSize {
			return nil, apperr.NewInvalid("imagem maior que o limite permitido")
		}
		f, err := fileHdr.Open()
		if err != nil {
			return nil, apperr.NewInvalid("não foi possível abrir a imagem")
		}
		defer f.Close()
		imgBytes, err := io.ReadAll(f)
		if err != nil {
			return nil, apperr.NewInvalid("erro lendo a imagem")
		}
		text, err := totp.DecodeQRImage(imgBytes)
		if err != nil {
			return nil, apperr.NewInvalid("não foi possível ler o QR code na imagem enviada")
		}
		accounts, err := totp.ExtractAccounts(text)
		if err != nil {
			return nil, apperr.NewInvalid("o QR code não contém uma chave TOTP reconhecível")
		}
		return accounts, nil
	}

	raw := c.FormValue("secret")
	if raw == "" {
		return nil, apperr.NewInvalid("envie o campo 'qr' (print) ou 'secret' (texto)")
	}
	accounts, err := totp.ExtractAccounts(raw)
	if err != nil {
		return nil, apperr.NewInvalid("texto enviado não contém uma chave TOTP reconhecível")
	}
	return accounts, nil
}

// selectedAccountIndex reads the optional "account_index" form field, present
// only on the FE's resubmit-with-selection call. ok is false when the field is
// absent (the first call against an ambiguous QR — caller responds with the
// picker instead of guessing).
func selectedAccountIndex(c *fiber.Ctx, candidateCount int) (idx int, ok bool, err error) {
	raw := c.FormValue("account_index")
	if raw == "" {
		return 0, false, nil
	}
	idx, convErr := strconv.Atoi(raw)
	if convErr != nil || idx < 0 || idx >= candidateCount {
		return 0, false, apperr.NewInvalid("account_index inválido")
	}
	return idx, true, nil
}

// needsSelectionView tells the FE the upload was well-formed but ambiguous —
// show mfaAccountView.Label per candidate and resubmit with "account_index".
// Deliberately carries no secret material.
type needsSelectionView struct {
	NeedsSelection bool             `json:"needs_selection"`
	Candidates     []mfaAccountView `json:"candidates"`
}

type mfaAccountView struct {
	Index int    `json:"index"`
	Label string `json:"label"`
}

func newNeedsSelectionView(candidates []totp.Account) needsSelectionView {
	views := make([]mfaAccountView, len(candidates))
	for i, a := range candidates {
		views[i] = mfaAccountView{Index: i, Label: mfaAccountLabel(a, i)}
	}
	return needsSelectionView{NeedsSelection: true, Candidates: views}
}

// mfaAccountLabel renders a human-readable label from whatever the migration
// QR actually carried — Google Authenticator entries sometimes lack an issuer
// (older exports) or a name, so this falls back gracefully rather than
// showing an empty picker row.
func mfaAccountLabel(a totp.Account, index int) string {
	switch {
	case a.Issuer != "" && a.Name != "":
		return fmt.Sprintf("%s (%s)", a.Issuer, a.Name)
	case a.Issuer != "":
		return a.Issuer
	case a.Name != "":
		return a.Name
	default:
		return fmt.Sprintf("conta sem nome (%d)", index+1)
	}
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

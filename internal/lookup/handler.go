package lookup

import (
	"github.com/gofiber/fiber/v2"

	"github.com/jusassessoria/platform/lib/httpx"
)

// Handler is the lookup HTTP surface. It owns its routing; the api only composes
// by calling Register on a group that runs AuthUser (verified user, no tenant).
type Handler struct {
	registry RegistryLookup
}

// NewHandler wires the handler to a RegistryLookup port (the BrasilAPI client in
// production, a fake in tests).
func NewHandler(registry RegistryLookup) *Handler {
	return &Handler{registry: registry}
}

// Register mounts the lookup routes on r. Both are reads keyed by a path param;
// there is no body and no tenant — the caller is any signed-in onboarding user.
func (h *Handler) Register(r fiber.Router) {
	r.Get("/lookup/cnpj/:cnpj", h.lookupCNPJ)
	r.Get("/lookup/cep/:cep", h.lookupCEP)
}

// lookupCNPJ handles GET /v1/lookup/cnpj/:cnpj: normalize+validate the id (a bad
// format is 400 before any fetch), then resolve it through the registry port and
// return the Company. Provider outcomes arrive already mapped to typed errors, so
// WriteError renders the right status (404 not-found, 503 unavailable, …).
func (h *Handler) lookupCNPJ(c *fiber.Ctx) error {
	cnpj, err := normalizeCNPJ(c.Params("cnpj"))
	if err != nil {
		return httpx.WriteError(c, err)
	}

	company, err := h.registry.LookupCNPJ(c.UserContext(), cnpj)
	if err != nil {
		return httpx.WriteError(c, err)
	}

	return c.Status(fiber.StatusOK).JSON(company)
}

// lookupCEP handles GET /v1/lookup/cep/:cep with the same shape: validate, then
// resolve through the port and return the Address.
func (h *Handler) lookupCEP(c *fiber.Ctx) error {
	cep, err := normalizeCEP(c.Params("cep"))
	if err != nil {
		return httpx.WriteError(c, err)
	}

	address, err := h.registry.LookupCEP(c.UserContext(), cep)
	if err != nil {
		return httpx.WriteError(c, err)
	}

	return c.Status(fiber.StatusOK).JSON(address)
}

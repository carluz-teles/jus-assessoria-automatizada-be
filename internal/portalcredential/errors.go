package portalcredential

import "github.com/jusassessoria/platform/lib/apperr"

// Typed, HTTP-agnostic slice errors. The edge (lib/httpx) maps each apperr.Kind
// to a status; the domain and repository only ever see these values.
var (
	// ErrPortalCredentialNotFound — the caller has no credential configured for
	// the portal yet. The repository returns it instead of (nil, nil) so GET
	// answers a typed 404 ("not configured"), never an empty 200.
	ErrPortalCredentialNotFound = apperr.NewNotFound("credencial de portal não configurada")

	// ErrPortalRejectedCredential — the portal's own login form explicitly
	// rejected the (login, password) pair (confirmed against the real TJSP eproc
	// SSO: an invalid-credentials response re-renders the Keycloak login page
	// with a visible error, see portal_login.go). The Configure use case never
	// persists this pair as ACTIVE; the handler maps it to 400 (KindInvalid) —
	// it is bad input from the caller (a wrong password), not a fault of ours.
	ErrPortalRejectedCredential = apperr.NewInvalid("credencial de portal rejeitada: usuário ou senha inválidos")

	// ErrVaultNotConfigured — VAULT_KEK_BASE64 is unset, so the slice cannot seal
	// or open secrets. Surfaces as a typed 503 (the same Kind unconfigured
	// upstream deps use elsewhere in the repo, e.g. document's S3Enabled gate) —
	// the endpoint exists but the KEK was never provisioned in this environment.
	ErrVaultNotConfigured = apperr.NewUnavailable("cofre de credenciais indisponível: KEK não configurada", nil)
)

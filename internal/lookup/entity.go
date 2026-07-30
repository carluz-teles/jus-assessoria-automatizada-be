// Package lookup is the stateless vertical slice behind the onboarding wizard's
// step-2 auto-fetch: it proxies public registry lookups (CNPJ, CEP) so the front
// end never talks to the third-party provider directly. It owns no database — no
// repository, no sqlc — only a port to an external registry and the HTTP surface
// that exposes it. The endpoints run under AuthUser (a verified user, no tenant
// yet), because they fire before the escritório exists.
package lookup

// Address is the typed subset of a Brazilian postal address the wizard needs.
// The json tags are the response contract with the front end (snake_case, like
// the other v0 DTOs). A CNPJ lookup fills every field; a CEP lookup leaves
// Number and Complement empty (the postal database does not carry them).
type Address struct {
	CEP          string `json:"cep"`
	Street       string `json:"street"`
	Number       string `json:"number"`
	Complement   string `json:"complement"`
	Neighborhood string `json:"neighborhood"`
	City         string `json:"city"`
	State        string `json:"state"`
}

// Company is the typed subset of a CNPJ registration the wizard pre-fills the
// onboarding form with. It carries the registered address inline so the front end
// gets everything from one call.
type Company struct {
	CNPJ      string  `json:"cnpj"`
	LegalName string  `json:"legal_name"`
	TradeName string  `json:"trade_name"`
	Address   Address `json:"address"`
}

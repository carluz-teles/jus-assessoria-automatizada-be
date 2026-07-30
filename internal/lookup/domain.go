package lookup

import "context"

// RegistryLookup is the port to the external registry that answers CNPJ and CEP
// queries. The handler depends on this interface, never on the concrete
// BrasilAPIClient — so the slice is unit-testable against a fake and the provider
// can be swapped without touching the HTTP surface.
//
// Implementations translate every provider outcome into the project's typed
// errors (apperr): a malformed query is Invalid, an unknown record is NotFound,
// and any transport/upstream failure is Unavailable — the raw provider status or
// body never crosses this boundary.
type RegistryLookup interface {
	// LookupCNPJ resolves a normalized 14-digit CNPJ to its registration.
	LookupCNPJ(ctx context.Context, cnpj string) (Company, error)
	// LookupCEP resolves a normalized 8-digit CEP to its address.
	LookupCEP(ctx context.Context, cep string) (Address, error)
}

package lookup

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/jusassessoria/platform/lib/apperr"
)

const (
	// defaultBaseURL is BrasilAPI's root. The versioned paths (/cnpj/v1, /cep/v2)
	// are appended per call — v2 CEP consults several providers and returns the
	// richer address shape this slice maps.
	defaultBaseURL = "https://brasilapi.com.br/api"

	// defaultTimeout bounds every upstream call. A slow provider must not hold a
	// request goroutine open indefinitely; on expiry the call surfaces as 503.
	defaultTimeout = 5 * time.Second

	// defaultUserAgent identifies this backend to the provider (courtesy + easier
	// upstream debugging) rather than leaking Go's default client string.
	defaultUserAgent = "jus-assessoria-backend/0 (+lookup)"
)

// BrasilAPIClient is the production RegistryLookup: it queries BrasilAPI over
// HTTP and maps each response to the slice's entities and typed errors.
type BrasilAPIClient struct {
	httpClient *http.Client
	baseURL    string
	userAgent  string
}

var _ RegistryLookup = (*BrasilAPIClient)(nil)

// Option configures a BrasilAPIClient (functional options — the API grows one
// option at a time without breaking callers). Tests inject WithBaseURL to point
// at an httptest server and WithHTTPClient to force short timeouts.
type Option func(*BrasilAPIClient)

// WithBaseURL overrides the provider root (used by tests against httptest).
func WithBaseURL(baseURL string) Option {
	return func(c *BrasilAPIClient) { c.baseURL = baseURL }
}

// WithHTTPClient overrides the underlying *http.Client (timeouts, transport).
func WithHTTPClient(hc *http.Client) Option {
	return func(c *BrasilAPIClient) { c.httpClient = hc }
}

// WithUserAgent overrides the User-Agent sent upstream.
func WithUserAgent(ua string) Option {
	return func(c *BrasilAPIClient) { c.userAgent = ua }
}

// NewBrasilAPIClient builds the client with sensible defaults (a 5s-timeout
// http.Client, the public base URL and a self-identifying User-Agent), then
// applies any overrides.
func NewBrasilAPIClient(opts ...Option) *BrasilAPIClient {
	c := &BrasilAPIClient{
		httpClient: &http.Client{Timeout: defaultTimeout},
		baseURL:    defaultBaseURL,
		userAgent:  defaultUserAgent,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// LookupCNPJ fetches /cnpj/v1/{cnpj} and maps it to a Company. cnpj is assumed
// already normalized to 14 digits by the handler.
func (c *BrasilAPIClient) LookupCNPJ(ctx context.Context, cnpj string) (Company, error) {
	var dto cnpjResponse
	if err := c.get(ctx, "/cnpj/v1/"+cnpj, &dto); err != nil {
		return Company{}, err
	}
	return dto.toCompany(), nil
}

// LookupCEP fetches /cep/v2/{cep} and maps it to an Address. cep is assumed
// already normalized to 8 digits by the handler.
func (c *BrasilAPIClient) LookupCEP(ctx context.Context, cep string) (Address, error) {
	var dto cepResponse
	if err := c.get(ctx, "/cep/v2/"+cep, &dto); err != nil {
		return Address{}, err
	}
	return dto.toAddress(), nil
}

// get performs the GET and folds every outcome into the slice's typed errors:
// 200 → decode into out; 400 → ErrInvalidQuery; 404 → ErrNotFound; a timeout,
// network failure or any other upstream status → Unavailable (503). The
// provider's raw status/body never escapes — only the mapped Kind does.
func (c *BrasilAPIClient) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		// A bad URL is on us, not the provider — but from the caller's view the
		// lookup simply could not be performed, so 503 is the honest answer.
		return apperr.NewUnavailable("build registry request", err)
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return apperr.NewUnavailable("registry provider unreachable", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return apperr.NewUnavailable("decode registry response", err)
		}
		return nil
	case http.StatusBadRequest:
		return ErrInvalidQuery
	case http.StatusNotFound:
		return ErrNotFound
	default:
		return apperr.NewUnavailable(
			fmt.Sprintf("registry provider returned status %d", resp.StatusCode),
			nil,
		)
	}
}

// cnpjResponse is the subset of BrasilAPI's /cnpj/v1 payload this slice consumes.
// Unread fields are ignored by the decoder.
type cnpjResponse struct {
	CNPJ         string `json:"cnpj"`
	RazaoSocial  string `json:"razao_social"`
	NomeFantasia string `json:"nome_fantasia"`
	CEP          string `json:"cep"`
	Logradouro   string `json:"logradouro"`
	Numero       string `json:"numero"`
	Complemento  string `json:"complemento"`
	Bairro       string `json:"bairro"`
	Municipio    string `json:"municipio"`
	UF           string `json:"uf"`
}

// toCompany maps the provider payload to the slice entity — the one place the
// provider's Portuguese field names meet the slice's English contract.
func (r cnpjResponse) toCompany() Company {
	return Company{
		CNPJ:      r.CNPJ,
		LegalName: r.RazaoSocial,
		TradeName: r.NomeFantasia,
		Address: Address{
			CEP:          r.CEP,
			Street:       r.Logradouro,
			Number:       r.Numero,
			Complement:   r.Complemento,
			Neighborhood: r.Bairro,
			City:         r.Municipio,
			State:        r.UF,
		},
	}
}

// cepResponse is the subset of BrasilAPI's /cep/v2 payload this slice consumes.
type cepResponse struct {
	CEP          string `json:"cep"`
	State        string `json:"state"`
	City         string `json:"city"`
	Neighborhood string `json:"neighborhood"`
	Street       string `json:"street"`
}

// toAddress maps the postal payload to the slice entity. Number and Complement
// stay empty — the CEP database does not carry them.
func (r cepResponse) toAddress() Address {
	return Address{
		CEP:          r.CEP,
		Street:       r.Street,
		Neighborhood: r.Neighborhood,
		City:         r.City,
		State:        r.State,
	}
}

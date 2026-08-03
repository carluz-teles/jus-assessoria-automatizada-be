package calendar

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/jusassessoria/platform/lib/apperr"
)

// This mirrors the HTTP-client shape of internal/lookup (functional options,
// typed errors, a self-identifying User-Agent). It is a deliberate small
// re-code rather than a reuse: lib/ must not import a slice (dependency rule),
// and the concern differs (national holidays vs CNPJ/CEP lookup). BrasilAPI is
// already the project's registry provider, so the base URL and pattern are
// consistent with what the codebase already trusts.
const (
	brasilAPIBaseURL   = "https://brasilapi.com.br/api"
	brasilAPITimeout   = 10 * time.Second
	brasilAPIUserAgent = "jus-assessoria-backend/0 (+calendar)"
)

// BrasilAPIFetcher fetches national holidays from BrasilAPI's /feriados/v1/{ano}
// endpoint (fixed + movable holidays; national scope only — BrasilAPI has no
// state feed).
type BrasilAPIFetcher struct {
	httpClient *http.Client
	baseURL    string
	userAgent  string
}

var _ Fetcher = (*BrasilAPIFetcher)(nil)

// FetcherOption configures a BrasilAPIFetcher (functional options — tests inject
// WithFetcherBaseURL to point at an httptest server).
type FetcherOption func(*BrasilAPIFetcher)

// WithFetcherBaseURL overrides the provider root (used by tests).
func WithFetcherBaseURL(baseURL string) FetcherOption {
	return func(f *BrasilAPIFetcher) { f.baseURL = baseURL }
}

// WithFetcherHTTPClient overrides the underlying *http.Client.
func WithFetcherHTTPClient(hc *http.Client) FetcherOption {
	return func(f *BrasilAPIFetcher) { f.httpClient = hc }
}

// NewBrasilAPIFetcher builds the fetcher with sensible defaults, then applies
// any overrides.
func NewBrasilAPIFetcher(opts ...FetcherOption) *BrasilAPIFetcher {
	f := &BrasilAPIFetcher{
		httpClient: &http.Client{Timeout: brasilAPITimeout},
		baseURL:    brasilAPIBaseURL,
		userAgent:  brasilAPIUserAgent,
	}
	for _, opt := range opts {
		opt(f)
	}
	return f
}

// NationalHolidays fetches /feriados/v1/{year} and maps each entry to a NATIONAL
// Holiday. A timeout, network failure or non-200 status surfaces as an
// Unavailable error — the seeder treats it as a skippable year, never a boot
// failure.
func (f *BrasilAPIFetcher) NationalHolidays(ctx context.Context, year int) ([]Holiday, error) {
	url := fmt.Sprintf("%s/feriados/v1/%d", f.baseURL, year)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, apperr.NewUnavailable("build holiday request", err)
	}
	req.Header.Set("User-Agent", f.userAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := f.httpClient.Do(req)
	if err != nil {
		return nil, apperr.NewUnavailable("holiday provider unreachable", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, apperr.NewUnavailable(
			fmt.Sprintf("holiday provider returned status %d", resp.StatusCode), nil)
	}

	var dtos []feriadoDTO
	if err := json.NewDecoder(resp.Body).Decode(&dtos); err != nil {
		return nil, apperr.NewUnavailable("decode holiday response", err)
	}

	holidays := make([]Holiday, 0, len(dtos))
	for _, dto := range dtos {
		day, err := time.Parse(time.DateOnly, dto.Date)
		if err != nil {
			return nil, apperr.NewUnavailable(
				fmt.Sprintf("holiday provider returned unparseable date %q", dto.Date), err)
		}
		holidays = append(holidays, Holiday{
			Scope: ScopeNational,
			Date:  day,
			Name:  dto.Name,
		})
	}
	return holidays, nil
}

// feriadoDTO is the subset of BrasilAPI's /feriados/v1 payload we consume.
type feriadoDTO struct {
	Date string `json:"date"`
	Name string `json:"name"`
}

package lookup

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jusassessoria/platform/lib/apperr"
)

// newStubServer serves a single canned (status, body) for any path and records
// the path it was asked for, so a test can assert both the mapping and the URL.
func newStubServer(t *testing.T, status int, body string) (*httptest.Server, *string) {
	t.Helper()

	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &gotPath
}

func TestBrasilAPIClient_LookupCNPJ_OK(t *testing.T) {
	t.Parallel()

	const body = `{
		"cnpj": "19131243000197",
		"razao_social": "OPEN KNOWLEDGE BRASIL",
		"nome_fantasia": "REDE PELO CONHECIMENTO LIVRE",
		"cep": "01311902",
		"logradouro": "PAULISTA",
		"numero": "37",
		"complemento": "ANDAR 4",
		"bairro": "BELA VISTA",
		"municipio": "SAO PAULO",
		"uf": "SP"
	}`
	srv, gotPath := newStubServer(t, http.StatusOK, body)
	c := NewBrasilAPIClient(WithBaseURL(srv.URL))

	got, err := c.LookupCNPJ(context.Background(), "19131243000197")
	if err != nil {
		t.Fatalf("LookupCNPJ: %v", err)
	}

	if *gotPath != "/cnpj/v1/19131243000197" {
		t.Errorf("requested path = %q, want /cnpj/v1/19131243000197", *gotPath)
	}
	want := Company{
		CNPJ:      "19131243000197",
		LegalName: "OPEN KNOWLEDGE BRASIL",
		TradeName: "REDE PELO CONHECIMENTO LIVRE",
		Address: Address{
			CEP:          "01311902",
			Street:       "PAULISTA",
			Number:       "37",
			Complement:   "ANDAR 4",
			Neighborhood: "BELA VISTA",
			City:         "SAO PAULO",
			State:        "SP",
		},
	}
	if got != want {
		t.Errorf("company = %+v, want %+v", got, want)
	}
}

func TestBrasilAPIClient_LookupCEP_OK(t *testing.T) {
	t.Parallel()

	const body = `{
		"cep": "01311902",
		"state": "SP",
		"city": "São Paulo",
		"neighborhood": "Bela Vista",
		"street": "Avenida Paulista"
	}`
	srv, gotPath := newStubServer(t, http.StatusOK, body)
	c := NewBrasilAPIClient(WithBaseURL(srv.URL))

	got, err := c.LookupCEP(context.Background(), "01311902")
	if err != nil {
		t.Fatalf("LookupCEP: %v", err)
	}

	if *gotPath != "/cep/v2/01311902" {
		t.Errorf("requested path = %q, want /cep/v2/01311902", *gotPath)
	}
	want := Address{
		CEP:          "01311902",
		Street:       "Avenida Paulista",
		Neighborhood: "Bela Vista",
		City:         "São Paulo",
		State:        "SP",
		// Number/Complement are absent from the CEP database — stay empty.
	}
	if got != want {
		t.Errorf("address = %+v, want %+v", got, want)
	}
}

// lookupCase names one of the two endpoints so the status/timeout tables exercise
// both CNPJ and CEP through the same table (per AC7).
type lookupCase struct {
	name string
	call func(c *BrasilAPIClient, ctx context.Context) error
}

var lookupCases = []lookupCase{
	{
		name: "cnpj",
		call: func(c *BrasilAPIClient, ctx context.Context) error {
			_, err := c.LookupCNPJ(ctx, "19131243000197")
			return err
		},
	},
	{
		name: "cep",
		call: func(c *BrasilAPIClient, ctx context.Context) error {
			_, err := c.LookupCEP(ctx, "01311902")
			return err
		},
	},
}

func TestBrasilAPIClient_StatusMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		status   int
		wantKind apperr.Kind
	}{
		{name: "404 is not found", status: http.StatusNotFound, wantKind: apperr.KindNotFound},
		{name: "400 is invalid", status: http.StatusBadRequest, wantKind: apperr.KindInvalid},
		{name: "500 is unavailable", status: http.StatusInternalServerError, wantKind: apperr.KindUnavailable},
		{name: "502 is unavailable", status: http.StatusBadGateway, wantKind: apperr.KindUnavailable},
	}

	for _, endpoint := range lookupCases {
		for _, tt := range tests {
			t.Run(endpoint.name+"/"+tt.name, func(t *testing.T) {
				t.Parallel()

				srv, _ := newStubServer(t, tt.status, `{"message":"raw upstream detail"}`)
				c := NewBrasilAPIClient(WithBaseURL(srv.URL))

				err := endpoint.call(c, context.Background())
				assertKind(t, err, tt.wantKind)
			})
		}
	}
}

func TestBrasilAPIClient_TimeoutIsUnavailable(t *testing.T) {
	t.Parallel()

	for _, endpoint := range lookupCases {
		t.Run(endpoint.name, func(t *testing.T) {
			t.Parallel()

			// The server never answers within the client's timeout. close(block)
			// is deferred before srv.Close so the handler unblocks first and Close
			// does not deadlock waiting on the in-flight request.
			block := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				<-block
			}))
			defer srv.Close()
			defer close(block)

			c := NewBrasilAPIClient(
				WithBaseURL(srv.URL),
				WithHTTPClient(&http.Client{Timeout: 30 * time.Millisecond}),
			)

			err := endpoint.call(c, context.Background())
			assertKind(t, err, apperr.KindUnavailable)
		})
	}
}

func TestBrasilAPIClient_UnreachableIsUnavailable(t *testing.T) {
	t.Parallel()

	// Point at a server that is immediately closed: Do fails with a connection
	// error, which must map to Unavailable, not a panic or a 500.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	base := srv.URL
	srv.Close()

	c := NewBrasilAPIClient(WithBaseURL(base))
	_, err := c.LookupCNPJ(context.Background(), "19131243000197")
	assertKind(t, err, apperr.KindUnavailable)
}

// assertKind fails unless err carries an *AppError of the wanted kind.
func assertKind(t *testing.T, err error, want apperr.Kind) {
	t.Helper()

	if err == nil {
		t.Fatalf("got nil error, want kind %q", want)
	}
	ae, ok := apperr.From(err)
	if !ok {
		t.Fatalf("error %v is not an *AppError", err)
	}
	if ae.Kind != want {
		t.Fatalf("error kind = %q, want %q", ae.Kind, want)
	}
}

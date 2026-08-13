package indexing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jusassessoria/platform/lib/apperr"
)

// voyage_test.go exercises the Voyage adapter against an httptest server (NEVER the real API):
// the request shape (auth header, model, output_dimension, input_type), the response parse
// (vectors in index order), the empty-input no-op, and error mapping (non-2xx → infra).

func TestVoyageEmbedder_RequestAndResponse(t *testing.T) {
	t.Parallel()

	var gotAuth, gotCT string
	var gotBody voyageRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		// Return two vectors, out of order (index 1 first) to prove index-based reassembly.
		resp := voyageResponse{Model: "voyage-3.5-lite"}
		resp.Data = []struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		}{
			{Embedding: []float32{9, 9}, Index: 1},
			{Embedding: []float32{1, 1}, Index: 0},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	e := &VoyageEmbedder{apiKey: "sk-test", model: "voyage-3.5-lite", dim: 1024, client: srv.Client()}
	// Point the adapter at the test server by overriding the endpoint via a wrapper: we can't
	// change the const, so use a client with a rewriting transport instead.
	e.client = &http.Client{Transport: rewriteTo(srv.URL)}

	vecs, model, err := e.Embed(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("auth header = %q", gotAuth)
	}
	if gotCT != "application/json" {
		t.Errorf("content-type = %q", gotCT)
	}
	if gotBody.Model != "voyage-3.5-lite" || gotBody.OutputDimension != 1024 || gotBody.InputType != "document" {
		t.Errorf("request body wrong: %+v", gotBody)
	}
	if len(gotBody.Input) != 2 {
		t.Errorf("input len = %d, want 2", len(gotBody.Input))
	}
	if model != "voyage-3.5-lite" {
		t.Errorf("model = %q", model)
	}
	// Reassembled by index: index 0 → {1,1}, index 1 → {9,9}.
	if len(vecs) != 2 || vecs[0][0] != 1 || vecs[1][0] != 9 {
		t.Errorf("vectors not reassembled by index: %+v", vecs)
	}
}

func TestVoyageEmbedder_EmptyInputNoOp(t *testing.T) {
	t.Parallel()

	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		called = true
	}))
	defer srv.Close()

	e := &VoyageEmbedder{apiKey: "sk", model: "m", dim: 8, client: &http.Client{Transport: rewriteTo(srv.URL)}}
	vecs, model, err := e.Embed(context.Background(), nil)
	if err != nil || vecs != nil || model != "m" {
		t.Fatalf("empty input: vecs=%v model=%q err=%v", vecs, model, err)
	}
	if called {
		t.Error("empty input made an HTTP call")
	}
}

func TestVoyageEmbedder_Non2xxIsInfra(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	defer srv.Close()

	e := &VoyageEmbedder{apiKey: "sk", model: "m", dim: 8, client: &http.Client{Transport: rewriteTo(srv.URL)}}
	_, _, err := e.Embed(context.Background(), []string{"x"})
	if err == nil {
		t.Fatal("expected error on 500")
	}
	ae, ok := apperr.From(err)
	if !ok || ae.Kind != apperr.KindInfra {
		t.Errorf("err = %v, want KindInfra", err)
	}
}

func TestNewVoyageEmbedder_Validation(t *testing.T) {
	t.Parallel()

	if _, err := NewVoyageEmbedder("", "m", 1024, nil); err == nil {
		t.Error("empty api key should error")
	}
	if _, err := NewVoyageEmbedder("k", "", 1024, nil); err == nil {
		t.Error("empty model should error")
	}
	if _, err := NewVoyageEmbedder("k", "m", 0, nil); err == nil {
		t.Error("zero dim should error")
	}
	if _, err := NewVoyageEmbedder("k", "m", 1024, nil); err != nil {
		t.Errorf("valid config errored: %v", err)
	}
}

// rewriteTo returns an http.RoundTripper that redirects every request to base (the test server),
// preserving the path — so the adapter's hardcoded voyageEndpoint is transparently pointed at
// httptest without changing the const.
func rewriteTo(base string) http.RoundTripper {
	return &rewriteTransport{base: base}
}

type rewriteTransport struct{ base string }

func (rt *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	u, err := http.NewRequestWithContext(req.Context(), req.Method, rt.base, req.Body)
	if err != nil {
		return nil, err
	}
	u.Header = req.Header
	return http.DefaultTransport.RoundTrip(u)
}

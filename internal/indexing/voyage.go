package indexing

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/jusassessoria/platform/lib/apperr"
)

// voyage.go is the concrete Embedder — the Voyage AI embeddings adapter, stdlib net/http only (no
// new dependency). It is kept behind the Embedder port (ports.go) so the pipeline never depends
// on it directly and tests inject a fake — the real API is NEVER called under test.

// voyageEndpoint is the Voyage embeddings API. output_dimension pins the vector width to the
// milestone's 1024 (voyage-3.5-lite's native dim), input_type "document" tells Voyage these are
// corpus documents (the retrieval query would use "query").
const voyageEndpoint = "https://api.voyageai.com/v1/embeddings"

// VoyageEmbedder embeds texts via the Voyage API. It holds the API key, the model id and the
// target dimensionality (from config), plus an *http.Client (injectable for tests, though tests
// use a fake Embedder — this client is only exercised by an integration test against the real
// API). Safe for concurrent use (http.Client is).
type VoyageEmbedder struct {
	apiKey string
	model  string
	dim    int
	client *http.Client
}

var _ Embedder = (*VoyageEmbedder)(nil)

// NewVoyageEmbedder builds the adapter from config. A missing API key is a caller mistake
// surfaced at construction as an apperr.Invalid (the worker fails fast at wiring, never silently
// no-ops). model/dim default in config (voyage-3.5-lite / 1024). The http.Client is the caller's
// (share one across the worker) or nil to use http.DefaultClient.
func NewVoyageEmbedder(apiKey, model string, dim int, client *http.Client) (*VoyageEmbedder, error) {
	if apiKey == "" {
		return nil, apperr.NewInvalid("voyage: api key is required")
	}
	if model == "" {
		return nil, apperr.NewInvalid("voyage: model is required")
	}
	if dim <= 0 {
		return nil, apperr.NewInvalid("voyage: embed dim must be positive")
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &VoyageEmbedder{apiKey: apiKey, model: model, dim: dim, client: client}, nil
}

// voyageRequest is the POST body: the batch of texts, the model, the pinned output dimension and
// the input type. Voyage accepts arrays, so the whole chunk batch goes in one call.
type voyageRequest struct {
	Input           []string `json:"input"`
	Model           string   `json:"model"`
	OutputDimension int      `json:"output_dimension"`
	InputType       string   `json:"input_type"`
}

// voyageResponse is the parsed reply: data[].embedding (one per input, in order) and the model
// echoed back (returned as the embedding_model provenance).
type voyageResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
		Index     int       `json:"index"`
	} `json:"data"`
	Model string `json:"model"`
}

// Embed POSTs texts to Voyage in one call and returns the vectors (in input order), the model id
// (for chunk.embedding_model provenance) and an error. An empty input is a no-op (no HTTP call).
// A non-2xx or a body-read/decode fault is an infra error (retryable — a transient Voyage/network
// blip); the pipeline's isTerminal keeps these on the retry path.
func (e *VoyageEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, string, error) {
	if len(texts) == 0 {
		return nil, e.model, nil
	}

	body, err := json.Marshal(voyageRequest{
		Input:           texts,
		Model:           e.model,
		OutputDimension: e.dim,
		InputType:       "document",
	})
	if err != nil {
		return nil, "", apperr.NewInfra("voyage: marshal request", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, voyageEndpoint, bytes.NewReader(body))
	if err != nil {
		return nil, "", apperr.NewInfra("voyage: build request", err)
	}
	req.Header.Set("Authorization", "Bearer "+e.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, "", apperr.NewInfra("voyage: request failed", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", apperr.NewInfra("voyage: read response", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", apperr.NewInfra("voyage: unexpected status", fmt.Errorf("status %d: %s", resp.StatusCode, string(respBody)))
	}

	var parsed voyageResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, "", apperr.NewInfra("voyage: decode response", err)
	}

	// Voyage returns data in input order and echoes the index; sort defensively is unnecessary
	// (the API preserves order) but guard the count so a partial reply surfaces as a clear error
	// rather than a silent short batch the pipeline would mis-align.
	if len(parsed.Data) != len(texts) {
		return nil, "", apperr.NewInfra("voyage: response length mismatch", fmt.Errorf("got %d embeddings for %d inputs", len(parsed.Data), len(texts)))
	}

	vectors := make([][]float32, len(parsed.Data))
	for _, d := range parsed.Data {
		if d.Index < 0 || d.Index >= len(vectors) {
			return nil, "", apperr.NewInfra("voyage: bad response index", fmt.Errorf("index %d out of range", d.Index))
		}
		vectors[d.Index] = d.Embedding
	}

	model := parsed.Model
	if model == "" {
		model = e.model
	}
	return vectors, model, nil
}

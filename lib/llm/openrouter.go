package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jusassessoria/platform/lib/apperr"
)

// openrouter.go is the concrete Generator — the OpenRouter chat-completions adapter, stdlib
// net/http only (no new dependency, like internal/indexing's Voyage embedder). It POSTs an
// OpenAI-compatible request with response_format=json_schema + provider.require_parameters so the
// model's reply is a JSON string matching the caller's schema, then returns that string's bytes.
// It is kept behind the Generator port so use cases never depend on it directly and tests inject a
// fake — the real API is NEVER called under test.

// defaultMaxTokens caps a generation when Request.MaxTokens is 0. 800 is comfortably above the
// small structured payloads this port serves (e.g. a handful of suggested tasks) while bounding
// cost/latency on a runaway completion.
const defaultMaxTokens = 800

// completionsPath is appended to the base URL to form the chat-completions endpoint. baseURL is
// e.g. https://openrouter.ai/api/v1, so the full URL is …/v1/chat/completions.
const completionsPath = "/chat/completions"

// maxAttempts is the total number of tries (1 initial + retries) on a TRANSIENT failure (HTTP 429
// or 5xx, or a network fault). 3 attempts with a short backoff rides out a provider blip without
// hanging the request; a 4xx (bad request/auth) is terminal and never retried.
const maxAttempts = 3

// OpenRouterGenerator generates structured JSON via OpenRouter's chat-completions API. It holds
// the API key, the default model id and the base URL (all from config), plus an *http.Client
// (injectable for tests). Safe for concurrent use (http.Client is; the struct is read-only after
// construction).
type OpenRouterGenerator struct {
	apiKey       string
	baseURL      string
	defaultModel string
	client       *http.Client
}

var _ Generator = (*OpenRouterGenerator)(nil)

// NewOpenRouterGenerator builds the adapter from config. A missing API key is a caller mistake
// surfaced at construction as an apperr.Invalid — so the binary that WANTS to generate fails loudly
// at wiring; a binary that has no key simply does not build this adapter (see cmd/api wiring, which
// injects nil when the key is unset). defaultModel/baseURL default in config. The http.Client is
// the caller's (share one) or nil to use http.DefaultClient.
func NewOpenRouterGenerator(apiKey, baseURL, defaultModel string, client *http.Client) (*OpenRouterGenerator, error) {
	if apiKey == "" {
		return nil, apperr.NewInvalid("openrouter: api key is required")
	}
	if baseURL == "" {
		return nil, apperr.NewInvalid("openrouter: base url is required")
	}
	if defaultModel == "" {
		return nil, apperr.NewInvalid("openrouter: default model is required")
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &OpenRouterGenerator{
		apiKey:       apiKey,
		baseURL:      strings.TrimRight(baseURL, "/"),
		defaultModel: defaultModel,
		client:       client,
	}, nil
}

// chatMessage is one OpenAI-compatible message (role + content).
type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// providerConfig carries OpenRouter's provider routing knobs. require_parameters=true tells
// OpenRouter to only route to providers that honor the response_format (structured output), so we
// never get a free-text reply back from a provider that silently drops the schema.
type providerConfig struct {
	RequireParameters bool `json:"require_parameters"`
}

// responseFormat pins the reply to a JSON Schema. json_schema.strict=true asks the model to match
// the schema exactly; name labels it (OpenRouter requires a name).
type responseFormat struct {
	Type       string         `json:"type"`
	JSONSchema jsonSchemaSpec `json:"json_schema"`
}

type jsonSchemaSpec struct {
	Name   string          `json:"name"`
	Strict bool            `json:"strict"`
	Schema json.RawMessage `json:"schema"`
}

// chatRequest is the POST body: the model, the two messages, the provider routing (require
// structured-output support), the response_format (JSON Schema) and the token cap.
type chatRequest struct {
	Model          string         `json:"model"`
	Messages       []chatMessage  `json:"messages"`
	Provider       providerConfig `json:"provider"`
	ResponseFormat responseFormat `json:"response_format"`
	MaxTokens      int            `json:"max_tokens"`
}

// chatResponse is the parsed reply. choices[0].message.content is a STRING containing the JSON
// that matches the schema — the caller unmarshals those bytes. usage carries token counts (kept
// for observability/cost, though this adapter does not surface it yet).
type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// GenerateJSON POSTs the prompt + schema to OpenRouter and returns the model's JSON content bytes.
// It retries on a TRANSIENT failure (HTTP 429 or 5xx, or a network/read fault) with a short
// backoff, up to maxAttempts; a 4xx (bad request/auth) is terminal (invalid, no retry). Exhausting
// the retries yields a retryable infra error. The response body is embedded in every error for
// debuggability.
func (g *OpenRouterGenerator) GenerateJSON(ctx context.Context, req Request) ([]byte, error) {
	model := req.Model
	if model == "" {
		model = g.defaultModel
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}

	body, err := json.Marshal(chatRequest{
		Model: model,
		Messages: []chatMessage{
			{Role: "system", Content: req.System},
			{Role: "user", Content: req.User},
		},
		Provider: providerConfig{RequireParameters: true},
		ResponseFormat: responseFormat{
			Type: "json_schema",
			JSONSchema: jsonSchemaSpec{
				Name:   req.SchemaName,
				Strict: true,
				Schema: req.Schema,
			},
		},
		MaxTokens: maxTokens,
	})
	if err != nil {
		return nil, apperr.NewInfra("openrouter: marshal request", err)
	}

	url := g.baseURL + completionsPath

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		content, retryable, err := g.doOnce(ctx, url, body)
		if err == nil {
			return content, nil
		}
		lastErr = err
		// A terminal error (4xx/parse) never retries; a retryable one backs off unless this
		// was the last attempt or the context is done.
		if !retryable {
			return nil, err
		}
		if attempt == maxAttempts {
			break
		}
		// Linear backoff (200ms, 400ms). Respect a cancelled context rather than sleeping through it.
		select {
		case <-ctx.Done():
			return nil, apperr.NewInfra("openrouter: context cancelled during backoff", ctx.Err())
		case <-time.After(time.Duration(attempt) * 200 * time.Millisecond):
		}
	}
	return nil, lastErr
}

// doOnce runs a single request attempt. It returns the JSON content bytes on success, or an error
// plus a retryable flag: a network fault, a body-read fault, or an HTTP 429/5xx is retryable
// (infra); a 4xx is terminal (invalid); a malformed/empty success body is a terminal infra fault.
func (g *OpenRouterGenerator) doOnce(ctx context.Context, url string, body []byte) ([]byte, bool, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, false, apperr.NewInfra("openrouter: build request", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+g.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := g.client.Do(httpReq)
	if err != nil {
		// A transport error (dial/timeout/reset) is a transient blip — retryable.
		return nil, true, apperr.NewInfra("openrouter: request failed", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, true, apperr.NewInfra("openrouter: read response", err)
	}

	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		// 429 (overloaded/rate-limited) and 5xx are TRANSIENT — retryable.
		return nil, true, apperr.NewInfra("openrouter: transient upstream error", fmt.Errorf("status %d: %s", resp.StatusCode, string(respBody)))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Any other non-2xx (4xx bad request/auth) is a terminal caller/config fault — no retry.
		return nil, false, apperr.NewInvalid(fmt.Sprintf("openrouter: bad request (status %d): %s", resp.StatusCode, string(respBody)))
	}

	var parsed chatResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, false, apperr.NewInfra("openrouter: decode response", fmt.Errorf("%w: body=%s", err, string(respBody)))
	}
	if len(parsed.Choices) == 0 || parsed.Choices[0].Message.Content == "" {
		return nil, false, apperr.NewInfra("openrouter: empty completion", fmt.Errorf("no choices/content in body=%s", string(respBody)))
	}
	return []byte(parsed.Choices[0].Message.Content), false, nil
}

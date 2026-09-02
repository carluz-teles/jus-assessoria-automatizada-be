package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jusassessoria/platform/lib/apperr"
	"go.opentelemetry.io/otel/trace"
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
	// recorder is the optional cost/usage telemetry sink — nil skips recording (same
	// nil-safe optional-dependency shape as every other optional port in this codebase,
	// e.g. draft.GenerateUseCase's emb/chunkPub). A recorder fault is logged, never
	// propagated: telemetry must never cost the caller its generation.
	recorder UsageRecorder
}

var _ Generator = (*OpenRouterGenerator)(nil)

// NewOpenRouterGenerator builds the adapter from config. A missing API key is a caller mistake
// surfaced at construction as an apperr.Invalid — so the binary that WANTS to generate fails loudly
// at wiring; a binary that has no key simply does not build this adapter (see cmd/api wiring, which
// injects nil when the key is unset). defaultModel/baseURL default in config. The http.Client is
// the caller's (share one) or nil to use http.DefaultClient. recorder may be nil (no usage/cost
// telemetry persisted).
func NewOpenRouterGenerator(apiKey, baseURL, defaultModel string, client *http.Client, recorder UsageRecorder) (*OpenRouterGenerator, error) {
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
		recorder:     recorder,
	}, nil
}

// recordUsage persists u as a UsageEvent attributed to req/model, best-effort: a recorder
// fault is logged and swallowed — cost telemetry must never fail the generation it describes.
// A nil recorder (not wired) or a zero-value usage (nothing parsed from the response) is a
// silent no-op.
func (g *OpenRouterGenerator) recordUsage(ctx context.Context, req Request, model string, u Usage, latency time.Duration) {
	if g.recorder == nil || u.TotalTokens == 0 {
		return
	}
	ev := UsageEvent{
		TenantID:  req.TenantID,
		UseCase:   req.UseCase,
		Provider:  "openrouter",
		Model:     model,
		Usage:     u,
		LatencyMs: latency.Milliseconds(),
		TraceID:   trace.SpanContextFromContext(ctx).TraceID().String(),
	}
	if err := g.recorder.RecordUsage(ctx, ev); err != nil {
		slog.WarnContext(ctx, "openrouter: record usage failed",
			slog.String("use_case", req.UseCase), slog.Any("error", err))
	}
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
	Temperature    float64        `json:"temperature,omitempty"`
	Stream         bool           `json:"stream,omitempty"`
}

// chatResponse is the parsed reply. choices[0].message.content is a STRING containing the JSON
// that matches the schema — the caller unmarshals those bytes. usage carries token counts and
// cost — OpenRouter includes it in every response by default (the older usage.include /
// stream_options.include_usage request opt-ins are deprecated), so no request change is needed
// to get it.
type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage usagePayload `json:"usage"`
}

// usagePayload is OpenRouter's usage object, shared by the sync response and the final SSE
// chunk of a stream.
type usagePayload struct {
	PromptTokens        int     `json:"prompt_tokens"`
	CompletionTokens    int     `json:"completion_tokens"`
	TotalTokens         int     `json:"total_tokens"`
	Cost                float64 `json:"cost"`
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

// toUsage converts the wire payload to the port's Usage type.
func (p usagePayload) toUsage() Usage {
	return Usage{
		PromptTokens:     p.PromptTokens,
		CompletionTokens: p.CompletionTokens,
		TotalTokens:      p.TotalTokens,
		CachedTokens:     p.PromptTokensDetails.CachedTokens,
		CostUSD:          p.Cost,
	}
}

// GenerateJSONStream é a variante SSE do GenerateJSON. Faz o POST com
// `stream=true`, parseia o corpo text/event-stream do OpenAI-compat, invoca
// onChunk(delta) pra cada pedaço de conteúdo do modelo, e no fim retorna o
// content completo (idêntico ao que GenerateJSON devolveria). Retries seguem
// a mesma lógica do GenerateJSON — porém, uma vez que a stream começou a
// enviar chunks, uma queda no meio devolve o content parcial + erro.
func (g *OpenRouterGenerator) GenerateJSONStream(ctx context.Context, req Request, onChunk func(chunk string) error) ([]byte, error) {
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
		MaxTokens:   maxTokens,
		Temperature: req.Temperature,
		Stream:      true,
	})
	if err != nil {
		return nil, apperr.NewInfra("openrouter: marshal stream request", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, g.baseURL+completionsPath, bytes.NewReader(body))
	if err != nil {
		return nil, apperr.NewInfra("openrouter: build stream request", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+g.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := g.client.Do(httpReq)
	if err != nil {
		return nil, apperr.NewInfra("openrouter: stream request failed", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, apperr.NewInfra("openrouter: stream non-2xx",
			fmt.Errorf("status %d: %s", resp.StatusCode, string(respBody)))
	}

	// Parse SSE inline. Cada evento é `data: {json}\n` seguido de linha em
	// branco. `data: [DONE]\n` fecha o stream. Cada json tem
	// choices[0].delta.content — o delta parcial que juntamos. O usage (tokens/cost)
	// chega no ÚLTIMO evento de conteúdo, que tipicamente tem choices=[] — por isso o
	// usage é lido ANTES do `continue` de choices vazio, senão o custo do streaming
	// nunca é capturado.
	var full strings.Builder
	var usage Usage
	var finishReason string
	start := time.Now()
	reader := &sseReader{buf: make([]byte, 0, 4096), src: resp.Body}
	for {
		event, err := reader.next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			return []byte(full.String()), apperr.NewInfra("openrouter: stream read", err)
		}
		if event == "" || event == "[DONE]" {
			if event == "[DONE]" {
				break
			}
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage usagePayload `json:"usage"`
		}
		if err := json.Unmarshal([]byte(event), &chunk); err != nil {
			// Alguns providers mandam eventos comentários (`: ping`) ou não-JSON — ignora.
			continue
		}
		if chunk.Usage.TotalTokens > 0 {
			usage = chunk.Usage.toUsage()
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		if fr := chunk.Choices[0].FinishReason; fr != "" {
			finishReason = fr
		}
		delta := chunk.Choices[0].Delta.Content
		if delta == "" {
			continue
		}
		full.WriteString(delta)
		if onChunk != nil {
			if err := onChunk(delta); err != nil {
				return []byte(full.String()), err
			}
		}
	}
	g.recordUsage(ctx, req, model, usage, time.Since(start))
	slog.InfoContext(ctx, "openrouter stream finished",
		slog.String("use_case", req.UseCase),
		slog.String("finish_reason", finishReason),
		slog.Int("raw_len", full.Len()),
		slog.Int("completion_tokens", usage.CompletionTokens))
	return []byte(full.String()), nil
}

// sseReader lê blocos "data: X\n\n" do body. Usado internamente pra parsear
// text/event-stream sem pull de dep externa.
type sseReader struct {
	buf []byte
	src io.Reader
	eof bool
}

// next devolve o payload do próximo `data:` (sem prefixo, sem newline). "" se
// heartbeat/comentário. io.EOF quando body fecha.
func (r *sseReader) next(ctx context.Context) (string, error) {
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		// procura por "\n\n" no buffer (fim de um evento SSE)
		if idx := bytes.Index(r.buf, []byte("\n\n")); idx >= 0 {
			raw := r.buf[:idx]
			r.buf = r.buf[idx+2:]
			// concatena linhas `data: X` do evento
			var payload strings.Builder
			for _, line := range bytes.Split(raw, []byte("\n")) {
				line = bytes.TrimRight(line, "\r")
				if bytes.HasPrefix(line, []byte("data:")) {
					payload.Write(bytes.TrimSpace(line[5:]))
				}
			}
			return payload.String(), nil
		}
		if r.eof {
			return "", io.EOF
		}
		// lê mais bytes
		tmp := make([]byte, 4096)
		n, err := r.src.Read(tmp)
		if n > 0 {
			r.buf = append(r.buf, tmp[:n]...)
		}
		if err == io.EOF {
			r.eof = true
			continue
		}
		if err != nil {
			return "", err
		}
	}
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
		MaxTokens:   maxTokens,
		Temperature: req.Temperature,
	})
	if err != nil {
		return nil, apperr.NewInfra("openrouter: marshal request", err)
	}

	url := g.baseURL + completionsPath

	start := time.Now()
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		content, usage, retryable, err := g.doOnce(ctx, url, body)
		if err == nil {
			g.recordUsage(ctx, req, model, usage, time.Since(start))
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

// doOnce runs a single request attempt. It returns the JSON content bytes and the parsed usage on
// success, or an error plus a retryable flag: a network fault, a body-read fault, or an HTTP
// 429/5xx is retryable (infra); a 4xx is terminal (invalid); a malformed/empty success body is a
// terminal infra fault.
func (g *OpenRouterGenerator) doOnce(ctx context.Context, url string, body []byte) ([]byte, Usage, bool, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, Usage{}, false, apperr.NewInfra("openrouter: build request", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+g.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := g.client.Do(httpReq)
	if err != nil {
		// A transport error (dial/timeout/reset) is a transient blip — retryable.
		return nil, Usage{}, true, apperr.NewInfra("openrouter: request failed", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, Usage{}, true, apperr.NewInfra("openrouter: read response", err)
	}

	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		// 429 (overloaded/rate-limited) and 5xx are TRANSIENT — retryable.
		return nil, Usage{}, true, apperr.NewInfra("openrouter: transient upstream error", fmt.Errorf("status %d: %s", resp.StatusCode, string(respBody)))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Any other non-2xx (4xx bad request/auth) is a terminal caller/config fault — no retry.
		return nil, Usage{}, false, apperr.NewInvalid(fmt.Sprintf("openrouter: bad request (status %d): %s", resp.StatusCode, string(respBody)))
	}

	var parsed chatResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, Usage{}, false, apperr.NewInfra("openrouter: decode response", fmt.Errorf("%w: body=%s", err, string(respBody)))
	}
	if len(parsed.Choices) == 0 || parsed.Choices[0].Message.Content == "" {
		return nil, Usage{}, false, apperr.NewInfra("openrouter: empty completion", fmt.Errorf("no choices/content in body=%s", string(respBody)))
	}
	return []byte(parsed.Choices[0].Message.Content), parsed.Usage.toUsage(), false, nil
}

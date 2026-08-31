package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/jusassessoria/platform/lib/apperr"
)

// openrouter_test.go exercises the OpenRouter adapter against an httptest server (NEVER the real
// API): the request shape (auth header, model, response_format=json_schema, provider.require_
// parameters, max_tokens), the response parse (content string → bytes), the 429-retry-then-give-up
// path, and error typing (4xx → invalid/terminal, 5xx-exhausted → infra).

// capturedRequest is the decoded body the test server sees.
type capturedRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	Provider struct {
		RequireParameters bool `json:"require_parameters"`
	} `json:"provider"`
	ResponseFormat struct {
		Type       string `json:"type"`
		JSONSchema struct {
			Name   string          `json:"name"`
			Strict bool            `json:"strict"`
			Schema json.RawMessage `json:"schema"`
		} `json:"json_schema"`
	} `json:"response_format"`
	MaxTokens int `json:"max_tokens"`
}

func TestOpenRouterGenerator_RequestAndResponse(t *testing.T) {
	t.Parallel()

	var gotAuth, gotCT string
	var gotBody capturedRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		// The completion content is a STRING containing JSON matching the schema.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"tasks\":[{\"title\":\"Analisar\",\"kind\":\"ANALISE\"}]}"}}],"usage":{"total_tokens":42}}`))
	}))
	defer srv.Close()

	g, err := NewOpenRouterGenerator("sk-test", srv.URL, "openai/gpt-4o-mini", srv.Client(), nil)
	if err != nil {
		t.Fatalf("NewOpenRouterGenerator: %v", err)
	}

	schema := json.RawMessage(`{"type":"object","additionalProperties":false,"required":["tasks"],"properties":{"tasks":{"type":"array"}}}`)
	out, err := g.GenerateJSON(context.Background(), Request{
		System:     "sys",
		User:       "usr",
		Schema:     schema,
		SchemaName: "suggested_tasks",
	})
	if err != nil {
		t.Fatalf("GenerateJSON: %v", err)
	}

	if gotAuth != "Bearer sk-test" {
		t.Errorf("auth header = %q", gotAuth)
	}
	if gotCT != "application/json" {
		t.Errorf("content-type = %q", gotCT)
	}
	if gotBody.Model != "openai/gpt-4o-mini" {
		t.Errorf("model = %q", gotBody.Model)
	}
	if !gotBody.Provider.RequireParameters {
		t.Error("provider.require_parameters not set")
	}
	if gotBody.ResponseFormat.Type != "json_schema" || gotBody.ResponseFormat.JSONSchema.Name != "suggested_tasks" || !gotBody.ResponseFormat.JSONSchema.Strict {
		t.Errorf("response_format wrong: %+v", gotBody.ResponseFormat)
	}
	if gotBody.MaxTokens != defaultMaxTokens {
		t.Errorf("max_tokens = %d, want default %d", gotBody.MaxTokens, defaultMaxTokens)
	}
	if len(gotBody.Messages) != 2 || gotBody.Messages[0].Role != "system" || gotBody.Messages[1].Role != "user" {
		t.Errorf("messages wrong: %+v", gotBody.Messages)
	}

	// The returned bytes are the content STRING, unmarshalable into the caller's type.
	var payload struct {
		Tasks []struct {
			Title string `json:"title"`
			Kind  string `json:"kind"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("unmarshal content: %v (raw=%s)", err, out)
	}
	if len(payload.Tasks) != 1 || payload.Tasks[0].Title != "Analisar" || payload.Tasks[0].Kind != "ANALISE" {
		t.Errorf("parsed content = %+v", payload.Tasks)
	}
}

// fakeRecorder captures the last UsageEvent it was handed, and returns err on every call
// when set — used to assert both the happy-path capture and the best-effort-on-failure
// contract (a recorder fault must never fail the generation it describes).
type fakeRecorder struct {
	got UsageEvent
	err error
}

func (r *fakeRecorder) RecordUsage(_ context.Context, ev UsageEvent) error {
	r.got = ev
	return r.err
}

func TestOpenRouterGenerator_RecordsUsageAndCost(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{}"}}],"usage":{` +
			`"prompt_tokens":100,"completion_tokens":50,"total_tokens":150,"cost":0.001234,` +
			`"prompt_tokens_details":{"cached_tokens":10}}}`))
	}))
	defer srv.Close()

	rec := &fakeRecorder{}
	g, err := NewOpenRouterGenerator("k", srv.URL, "openai/gpt-4o-mini", srv.Client(), rec)
	if err != nil {
		t.Fatalf("NewOpenRouterGenerator: %v", err)
	}

	if _, err := g.GenerateJSON(context.Background(), Request{
		SchemaName: "s",
		Schema:     json.RawMessage(`{}`),
		UseCase:    "draft.generate",
		TenantID:   "tenant-1",
	}); err != nil {
		t.Fatalf("GenerateJSON: %v", err)
	}

	if rec.got.TenantID != "tenant-1" || rec.got.UseCase != "draft.generate" {
		t.Errorf("usage event attribution = %+v", rec.got)
	}
	if rec.got.Provider != "openrouter" || rec.got.Model != "openai/gpt-4o-mini" {
		t.Errorf("usage event provider/model = %+v", rec.got)
	}
	wantUsage := Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150, CachedTokens: 10, CostUSD: 0.001234}
	if rec.got.Usage != wantUsage {
		t.Errorf("usage = %+v, want %+v", rec.got.Usage, wantUsage)
	}
}

func TestOpenRouterGenerator_RecorderFailureDoesNotFailGeneration(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"ok\":true}"}}],"usage":{"total_tokens":1}}`))
	}))
	defer srv.Close()

	rec := &fakeRecorder{err: errors.New("insert failed")}
	g, _ := NewOpenRouterGenerator("k", srv.URL, "m", srv.Client(), rec)

	out, err := g.GenerateJSON(context.Background(), Request{SchemaName: "s", Schema: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("GenerateJSON should succeed despite a recorder failure: %v", err)
	}
	if string(out) != `{"ok":true}` {
		t.Errorf("content = %s", out)
	}
}

func TestOpenRouterGenerator_ModelOverrideAndMaxTokens(t *testing.T) {
	t.Parallel()

	var gotBody capturedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{}"}}]}`))
	}))
	defer srv.Close()

	g, _ := NewOpenRouterGenerator("k", srv.URL, "default-model", srv.Client(), nil)
	if _, err := g.GenerateJSON(context.Background(), Request{Model: "override/model", MaxTokens: 128, SchemaName: "s", Schema: json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("GenerateJSON: %v", err)
	}
	if gotBody.Model != "override/model" {
		t.Errorf("model override not applied: %q", gotBody.Model)
	}
	if gotBody.MaxTokens != 128 {
		t.Errorf("max_tokens override not applied: %d", gotBody.MaxTokens)
	}
}

func TestOpenRouterGenerator_RetriesOn429ThenGivesUp(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"rate limited"}`))
	}))
	defer srv.Close()

	g, _ := NewOpenRouterGenerator("k", srv.URL, "m", srv.Client(), nil)
	_, err := g.GenerateJSON(context.Background(), Request{SchemaName: "s", Schema: json.RawMessage(`{}`)})
	if err == nil {
		t.Fatal("expected error after exhausting retries on 429")
	}
	if got := calls.Load(); got != maxAttempts {
		t.Errorf("attempts = %d, want %d (retried)", got, maxAttempts)
	}
	ae, ok := apperr.From(err)
	if !ok || ae.Kind != apperr.KindInfra {
		t.Errorf("err = %v, want KindInfra (retryable)", err)
	}
}

func TestOpenRouterGenerator_RetriesThenSucceeds(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"overloaded"}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"ok\":true}"}}]}`))
	}))
	defer srv.Close()

	g, _ := NewOpenRouterGenerator("k", srv.URL, "m", srv.Client(), nil)
	out, err := g.GenerateJSON(context.Background(), Request{SchemaName: "s", Schema: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("expected success after one retry: %v", err)
	}
	if string(out) != `{"ok":true}` {
		t.Errorf("content = %s", out)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("attempts = %d, want 2 (one retry)", got)
	}
}

func TestOpenRouterGenerator_4xxIsTerminalInvalidNoRetry(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"bad key"}`))
	}))
	defer srv.Close()

	g, _ := NewOpenRouterGenerator("k", srv.URL, "m", srv.Client(), nil)
	_, err := g.GenerateJSON(context.Background(), Request{SchemaName: "s", Schema: json.RawMessage(`{}`)})
	if err == nil {
		t.Fatal("expected error on 401")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("attempts = %d, want 1 (no retry on 4xx)", got)
	}
	ae, ok := apperr.From(err)
	if !ok || ae.Kind != apperr.KindInvalid {
		t.Errorf("err = %v, want KindInvalid (terminal)", err)
	}
}

// TestOpenRouterGenerator_StreamRecordsUsageFromFinalChunk guards the streaming usage
// capture: OpenRouter's usage arrives on the LAST SSE event, which typically carries an
// EMPTY choices array — a naive parser that `continue`s on empty choices before checking
// usage would silently drop the cost of every streamed call.
func TestOpenRouterGenerator_StreamRecordsUsageFromFinalChunk(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15,\"cost\":0.0005}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	rec := &fakeRecorder{}
	g, _ := NewOpenRouterGenerator("k", srv.URL, "m", srv.Client(), rec)

	out, err := g.GenerateJSONStream(context.Background(), Request{
		SchemaName: "s",
		Schema:     json.RawMessage(`{}`),
		UseCase:    "draft.chat",
		TenantID:   "tenant-1",
	}, nil)
	if err != nil {
		t.Fatalf("GenerateJSONStream: %v", err)
	}
	if string(out) != "hi" {
		t.Errorf("content = %s", out)
	}

	wantUsage := Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15, CostUSD: 0.0005}
	if rec.got.Usage != wantUsage {
		t.Errorf("usage = %+v, want %+v", rec.got.Usage, wantUsage)
	}
	if rec.got.UseCase != "draft.chat" || rec.got.TenantID != "tenant-1" {
		t.Errorf("usage event attribution = %+v", rec.got)
	}
}

func TestNewOpenRouterGenerator_Validation(t *testing.T) {
	t.Parallel()

	if _, err := NewOpenRouterGenerator("", "https://x", "m", nil, nil); err == nil {
		t.Error("empty api key should error")
	}
	if _, err := NewOpenRouterGenerator("k", "", "m", nil, nil); err == nil {
		t.Error("empty base url should error")
	}
	if _, err := NewOpenRouterGenerator("k", "https://x", "", nil, nil); err == nil {
		t.Error("empty default model should error")
	}
	if _, err := NewOpenRouterGenerator("k", "https://x", "m", nil, nil); err != nil {
		t.Errorf("valid config errored: %v", err)
	}
}

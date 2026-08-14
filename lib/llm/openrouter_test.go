package llm

import (
	"context"
	"encoding/json"
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

	g, err := NewOpenRouterGenerator("sk-test", srv.URL, "openai/gpt-4o-mini", srv.Client())
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

func TestOpenRouterGenerator_ModelOverrideAndMaxTokens(t *testing.T) {
	t.Parallel()

	var gotBody capturedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{}"}}]}`))
	}))
	defer srv.Close()

	g, _ := NewOpenRouterGenerator("k", srv.URL, "default-model", srv.Client())
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

	g, _ := NewOpenRouterGenerator("k", srv.URL, "m", srv.Client())
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

	g, _ := NewOpenRouterGenerator("k", srv.URL, "m", srv.Client())
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

	g, _ := NewOpenRouterGenerator("k", srv.URL, "m", srv.Client())
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

func TestNewOpenRouterGenerator_Validation(t *testing.T) {
	t.Parallel()

	if _, err := NewOpenRouterGenerator("", "https://x", "m", nil); err == nil {
		t.Error("empty api key should error")
	}
	if _, err := NewOpenRouterGenerator("k", "", "m", nil); err == nil {
		t.Error("empty base url should error")
	}
	if _, err := NewOpenRouterGenerator("k", "https://x", "", nil); err == nil {
		t.Error("empty default model should error")
	}
	if _, err := NewOpenRouterGenerator("k", "https://x", "m", nil); err != nil {
		t.Errorf("valid config errored: %v", err)
	}
}

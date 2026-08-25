// Package llm is the reusable LLM generation client — the GENERATION counterpart of
// internal/indexing's Voyage embedder. It exposes a narrow Generator port over an
// OpenAI-compatible provider (OpenRouter) constrained to a JSON Schema, so callers get
// structured JSON back and unmarshal it into their own type. Pure stdlib net/http (no new
// dependency), mirroring the Voyage adapter's shape.
package llm

import (
	"context"
	"encoding/json"
)

// Generator is the generation port: given a prompt + a JSON Schema, it returns the model's
// JSON content bytes (constrained to the schema) for the caller to unmarshal. It is kept an
// interface so use cases depend on it (not the concrete OpenRouter adapter) and tests inject a
// fake — the real API is NEVER called under test.
type Generator interface {
	// GenerateJSON returns the model's JSON content bytes, constrained to req.Schema
	// (OpenRouter response_format=json_schema + provider.require_parameters). The caller
	// unmarshals the bytes into its own type.
	GenerateJSON(ctx context.Context, req Request) ([]byte, error)

	// GenerateJSONStream é a variante streaming (SSE). Chama onChunk pra cada
	// delta do modelo (texto UTF-8 parcial). No fim, devolve os bytes COMPLETOS
	// da resposta (idênticos aos que GenerateJSON devolveria) pro caller fazer
	// unmarshal do JSON estruturado. onChunk pode retornar erro pra abortar
	// a stream cedo — o erro do onChunk é propagado como retorno da fn.
	//
	// Nota: chunks são parcelas do CONTENT — pra structured output, o content
	// é um único JSON string, então o consumidor pode escolher stream do
	// próprio value se souber a shape (ex.: draft_html é um único campo
	// string, então basicamente todo o content APÓS abertura `{"draft_html":"`
	// é a peça sendo digitada).
	GenerateJSONStream(ctx context.Context, req Request, onChunk func(chunk string) error) ([]byte, error)
}

// Request is one generation call: the system + user prompts, the JSON Schema to constrain the
// output, a name for that schema, an optional model override ("" → the adapter's default) and an
// optional max token cap (0 → a sane default).
type Request struct {
	System     string          // the system prompt (role/instructions)
	User       string          // the user prompt (the concrete input/context)
	Schema     json.RawMessage // the JSON Schema object for response_format.json_schema.schema
	SchemaName string          // e.g. "suggested_tasks" — response_format.json_schema.name
	Model      string          // optional override; "" → the adapter's default model
	MaxTokens  int             // 0 → a sane default (defaultMaxTokens)
}

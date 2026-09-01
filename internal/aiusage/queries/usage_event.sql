-- name: InsertUsageEvent :exec
-- Records one completed LLM call's cost/usage. Called best-effort by lib/llm's
-- OpenRouterGenerator, OUTSIDE any domain transaction — a failure here must never roll
-- back or fail the generation it describes (the caller logs-and-continues on error).
INSERT INTO ai_usage_event (
  tenant_id, use_case, provider, model,
  prompt_tokens, completion_tokens, total_tokens, cached_tokens,
  cost_usd, latency_ms, trace_id
) VALUES (
  $1, $2, $3, $4,
  $5, $6, $7, $8,
  $9, $10, $11
);

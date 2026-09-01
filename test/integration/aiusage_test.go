//go:build integration

// ai_usage_event integration test — proves internal/aiusage.Recorder (migration 0077,
// lib/llm.UsageRecorder implementation) against a real Postgres: RecordUsage persists one
// row with the exact tokens/cost/model/use_case it was handed.
package integration_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jusassessoria/platform/internal/aiusage"
	"github.com/jusassessoria/platform/lib/llm"
)

// countAIUsageEvent counts ai_usage_event rows for a tenant with the given use_case
// (owner query, RLS bypassed).
func countAIUsageEvent(t *testing.T, pool *pgxpool.Pool, tenantID, useCase string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM ai_usage_event WHERE tenant_id = $1 AND use_case = $2`,
		tenantID, useCase).Scan(&n); err != nil {
		t.Fatalf("count ai_usage_event: %v", err)
	}
	return n
}

// TestRecorder_RecordUsage_PersistsRow proves a completed LLM call's usage is persisted
// with the exact tokens/cost/model/use_case handed to RecordUsage.
func TestRecorder_RecordUsage_PersistsRow(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-aiusage", 0)

	rec := aiusage.NewRecorder(pool)
	ev := llm.UsageEvent{
		TenantID: tenantID,
		UseCase:  "draft.generate",
		Provider: "openrouter",
		Model:    "google/gemini-2.5-flash",
		Usage: llm.Usage{
			PromptTokens:     1200,
			CompletionTokens: 340,
			TotalTokens:      1540,
			CachedTokens:     100,
			CostUSD:          0.004321,
		},
		LatencyMs: 850,
		TraceID:   "trace-abc",
	}
	if err := rec.RecordUsage(ctx, ev); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}

	if got := countAIUsageEvent(t, pool, tenantID, "draft.generate"); got != 1 {
		t.Fatalf("ai_usage_event rows = %d, want 1", got)
	}

	var model string
	var promptTokens, completionTokens, totalTokens, cachedTokens, latencyMs int
	var costUSD float64
	var traceID string
	if err := pool.QueryRow(ctx,
		`SELECT model, prompt_tokens, completion_tokens, total_tokens, cached_tokens, cost_usd, latency_ms, trace_id
		 FROM ai_usage_event WHERE tenant_id = $1 AND use_case = $2`,
		tenantID, "draft.generate",
	).Scan(&model, &promptTokens, &completionTokens, &totalTokens, &cachedTokens, &costUSD, &latencyMs, &traceID); err != nil {
		t.Fatalf("scan ai_usage_event: %v", err)
	}

	if model != ev.Model || promptTokens != ev.Usage.PromptTokens || completionTokens != ev.Usage.CompletionTokens ||
		totalTokens != ev.Usage.TotalTokens || cachedTokens != ev.Usage.CachedTokens || latencyMs != int(ev.LatencyMs) ||
		traceID != ev.TraceID {
		t.Errorf("persisted row mismatch: model=%q prompt=%d completion=%d total=%d cached=%d latency=%d trace=%q",
			model, promptTokens, completionTokens, totalTokens, cachedTokens, latencyMs, traceID)
	}
	if diff := costUSD - ev.Usage.CostUSD; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("cost_usd = %v, want %v", costUSD, ev.Usage.CostUSD)
	}
}

// TestRecorder_RecordUsage_InvalidTenantID proves a malformed TenantID surfaces as an
// error instead of silently no-op-ing or panicking — the caller (OpenRouterGenerator)
// logs it and moves on, per the best-effort contract on llm.UsageRecorder.
func TestRecorder_RecordUsage_InvalidTenantID(t *testing.T) {
	pool := newPool(t)

	rec := aiusage.NewRecorder(pool)
	err := rec.RecordUsage(context.Background(), llm.UsageEvent{
		TenantID: "not-a-uuid",
		UseCase:  "draft.generate",
		Provider: "openrouter",
		Model:    "m",
	})
	if err == nil {
		t.Fatal("expected an error for a malformed tenant id")
	}
}

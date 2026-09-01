// Package aiusage is the cost/usage telemetry sink for LLM calls (lib/llm.UsageRecorder):
// one row per completed OpenRouter call, tagged with the calling use case and tenant. It is
// a pure write-only sink (no HTTP surface, no domain use case) — like internal/advisory,
// it is just enough package to hold the port's implementation. Reporting/aggregation is a
// later slice once there is a concrete need for it.
package aiusage

import (
	"context"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jusassessoria/platform/internal/aiusage/aiusagedb"
	"github.com/jusassessoria/platform/lib/llm"
)

// Recorder implements llm.UsageRecorder against the shared pool. It writes OUTSIDE any
// domain transaction — this is best-effort telemetry, never part of the Unit of Work the
// generation itself runs in.
type Recorder struct {
	q *aiusagedb.Queries
}

// NewRecorder binds the generated queries to pool. Injected wherever an
// llm.OpenRouterGenerator is constructed (cmd/api, cmd/worker-ai).
func NewRecorder(pool aiusagedb.DBTX) *Recorder {
	return &Recorder{q: aiusagedb.New(pool)}
}

var _ llm.UsageRecorder = (*Recorder)(nil)

// RecordUsage persists ev. A malformed TenantID (not a UUID) is a caller bug surfaced as
// an error — the caller (OpenRouterGenerator) logs it and moves on, per the best-effort
// contract on llm.UsageRecorder.
func (r *Recorder) RecordUsage(ctx context.Context, ev llm.UsageEvent) error {
	tenantID, err := uuid.Parse(ev.TenantID)
	if err != nil {
		return err
	}
	var cost pgtype.Numeric
	if err := cost.Scan(strconv.FormatFloat(ev.Usage.CostUSD, 'f', -1, 64)); err != nil {
		return err
	}
	var traceID *string
	if ev.TraceID != "" {
		traceID = &ev.TraceID
	}
	return r.q.InsertUsageEvent(ctx, aiusagedb.InsertUsageEventParams{
		TenantID:         tenantID,
		UseCase:          ev.UseCase,
		Provider:         ev.Provider,
		Model:            ev.Model,
		PromptTokens:     int32(ev.Usage.PromptTokens),
		CompletionTokens: int32(ev.Usage.CompletionTokens),
		TotalTokens:      int32(ev.Usage.TotalTokens),
		CachedTokens:     int32(ev.Usage.CachedTokens),
		CostUsd:          cost,
		LatencyMs:        int32(ev.LatencyMs),
		TraceID:          traceID,
	})
}

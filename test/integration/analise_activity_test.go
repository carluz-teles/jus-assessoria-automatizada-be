//go:build integration

// Process activity log integration test — proves the intimation-analysis producer
// (migration 0073 / internal/acquisition/activity.go) against a real Postgres: a
// successful AI analysis appends ONE process_activity_log row, in the SAME tx as the
// intimation's ai_* columns, carrying the court_record_id of the process that owns the
// analysed intimation.
package integration_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jusassessoria/platform/internal/acquisition"
	"github.com/jusassessoria/platform/internal/advisory"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
	"github.com/jusassessoria/platform/lib/llm"
)

// fakeAnaliseGen is a canned llm.Generator for the analysis integration test — no real
// LLM call, just a fixed schema-shaped JSON payload.
type fakeAnaliseGen struct{}

func (fakeAnaliseGen) GenerateJSON(context.Context, llm.Request) ([]byte, error) {
	return []byte(`{"summary":"Prazo de 15 dias para contestação.","providencias":[]}`), nil
}

func (fakeAnaliseGen) GenerateJSONStream(context.Context, llm.Request, func(string) error) ([]byte, error) {
	return []byte(`{"summary":"Prazo de 15 dias para contestação.","providencias":[]}`), nil
}

// countProcessActivityLog counts process_activity_log rows for a court record with the
// given event type (owner query, RLS bypassed).
func countProcessActivityLog(t *testing.T, pool *pgxpool.Pool, courtRecordID, eventType string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM process_activity_log WHERE court_record_id = $1 AND event_type = $2`,
		courtRecordID, eventType).Scan(&n); err != nil {
		t.Fatalf("count process_activity_log: %v", err)
	}
	return n
}

// TestAnalisar_LogsProcessActivity proves a successful (non-degraded) intimation
// analysis appends one INTIMATION_ANALYSIS_COMPLETED row to process_activity_log, tied
// to the court_record_id of the process the analysed intimation belongs to.
func TestAnalisar_LogsProcessActivity(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-analise-activity", 0)
	recordID, caseID := seedCourtRecordCNJ(t, pool, tenantID, "0001111-22.2023.8.26.0100")
	intimationID := seedIntimationReturningID(t, pool, tenantID, caseID, recordID)

	reader := acquisition.NewReadUseCase(acquisition.NewRepository(pool))
	store := acquisition.NewAnaliseStore(database.NewUnitOfWork(pool), events.NewOutbox())
	uc := acquisition.NewAnaliseUseCase(reader, advisory.NewTemplateComposer(), fakeAnaliseGen{}, store, "")

	view, err := uc.Analisar(ctx, tenantID, intimationID)
	if err != nil {
		t.Fatalf("Analisar: %v", err)
	}
	if view.Summary == "" {
		t.Fatal("summary = empty, want the fake generator's answer (not degraded)")
	}

	if got := countProcessActivityLog(t, pool, recordID, "INTIMATION_ANALYSIS_COMPLETED"); got != 1 {
		t.Errorf("process_activity_log rows = %d, want 1", got)
	}

	// The analysis also publishes acquisition.intimation.analyzed in the SAME tx (docs/erd-
	// costura-providencia-tarefa-peca.md) — the actionitem slice's listener materializes
	// action_item rows from it.
	if got := countRows(t, pool,
		`SELECT count(*) FROM outbox WHERE type=$1 AND payload->>'intimation_id'=$2`,
		acquisition.TypeIntimationAnalyzed, intimationID); got != 1 {
		t.Errorf("intimation.analyzed outbox rows = %d, want 1", got)
	}
}

// TestAnalisar_Degraded_DoesNotLogActivity proves the degraded write (no LLM configured)
// persists the intimation's ai_* columns but never appends a process_activity_log row —
// an "IA indisponível" write is not something to surface on the process timeline.
func TestAnalisar_Degraded_DoesNotLogActivity(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-analise-degraded", 0)
	recordID, caseID := seedCourtRecordCNJ(t, pool, tenantID, "0001111-22.2023.8.26.0200")
	intimationID := seedIntimationReturningID(t, pool, tenantID, caseID, recordID)

	reader := acquisition.NewReadUseCase(acquisition.NewRepository(pool))
	store := acquisition.NewAnaliseStore(database.NewUnitOfWork(pool), events.NewOutbox())
	// gen=nil → degraded path (no LLM configured).
	uc := acquisition.NewAnaliseUseCase(reader, advisory.NewTemplateComposer(), nil, store, "")

	view, err := uc.Analisar(ctx, tenantID, intimationID)
	if err != nil {
		t.Fatalf("Analisar: %v", err)
	}
	if view.Summary != "" {
		t.Fatalf("summary = %q, want empty (degraded)", view.Summary)
	}

	if got := countProcessActivityLog(t, pool, recordID, "INTIMATION_ANALYSIS_COMPLETED"); got != 0 {
		t.Errorf("process_activity_log rows = %d, want 0 (degraded write must not log)", got)
	}

	// The event STILL publishes on a degraded write (empty candidates) — the actionitem
	// listener's guard aditivo must run even when a re-run comes back empty.
	if got := countRows(t, pool,
		`SELECT count(*) FROM outbox WHERE type=$1 AND payload->>'intimation_id'=$2`,
		acquisition.TypeIntimationAnalyzed, intimationID); got != 1 {
		t.Errorf("intimation.analyzed outbox rows = %d, want 1 (degraded still publishes)", got)
	}
}

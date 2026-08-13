//go:build integration

// Intimation events integration tests — prove the acquisition sync cycle emits the
// intimation domain events in the SAME tx as the intimation upsert, against a real
// Postgres (the full jsonb_to_recordset round-trip the unit tests mock):
//   - a NEW intimação lands exactly one acquisition.intimation.observed whose payload
//     carries the denormalized uf (TJSP → SP), the record/case ids and the deadline;
//   - a re-sync that brings data_cancelamento transitions the row ACTIVE → CANCELLED
//     and lands exactly one acquisition.intimation.cancelled (with the reason), and NO
//     second observed.
package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jusassessoria/platform/internal/acquisition"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// scriptedParser is a Parser whose ParsedResult the test swaps between deliveries,
// so a second sync can bring the SAME intimação carrying a cancellation. It stands in
// for the real DJEN parser; the stub connector feeds it a canned payload.
type scriptedParser struct{ result acquisition.ParsedResult }

func (p *scriptedParser) CanParse(acquisition.RawPayload) bool { return true }

func (p *scriptedParser) Parse(context.Context, acquisition.RawPayload) (acquisition.ParsedResult, error) {
	return p.result, nil
}

// newSyncUCWithParser wires the sync use case like newSyncUC but with a caller-owned
// parser, so the test drives the parsed intimação (active, then cancelled).
func newSyncUCWithParser(pool *pgxpool.Pool, parser acquisition.Parser) *acquisition.SyncUseCase {
	orch := acquisition.NewOrchestrator()
	orch.Register(acquisition.SourceDJEN, acquisition.NewStubConnector(acquisition.SourceDJEN))
	return acquisition.NewSyncUseCase(
		acquisition.NewRepository(pool),
		events.NewOutbox(),
		database.NewUnitOfWork(pool),
		orch,
		parser,
	)
}

// intimationFixture builds a one-record, one-intimação parsed result. The court is
// TJSP (uf SP) so the observed event's denormalized uf is assertable; status/reason
// drive the ACTIVE → CANCELLED transition on a re-sync.
func intimationFixture(status, reason string) acquisition.ParsedResult {
	const cnj = "0000009-99.2024.8.26.0100"
	made := time.Date(2024, 1, 12, 0, 0, 0, 0, time.UTC)
	deadline := time.Date(2024, 1, 16, 0, 0, 0, 0, time.UTC)

	intim := acquisition.ParsedIntimation{
		CNJNumber:       cnj,
		Degree:          acquisition.DegreeG1,
		Hash:            "intim-evt-1",
		MadeAvailableAt: made,
		PublishedAt:     made,
		DeadlineStartAt: deadline,
		Content:         "intimação para manifestação",
		Source:          acquisition.SourceDJEN,
		Type:            acquisition.IntimationTypeIntimacao,
		Status:          status,
		CancelReason:    reason,
	}
	if status == acquisition.IntimationStatusCancelled {
		intim.CancelledAt = time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)
	}

	return acquisition.ParsedResult{
		CourtRecords: []acquisition.ParsedCourtRecord{{
			CNJNumber:    cnj,
			Degree:       acquisition.DegreeG1,
			Court:        "TJSP",
			Completeness: 0.5,
		}},
		Intimations: []acquisition.ParsedIntimation{intim},
	}
}

// II1: a first delivery of a fresh intimação commits exactly one
// acquisition.intimation.observed in the same tx, with the denormalized uf (SP), the
// record/case ids and the deadline, and its aggregate_id is the intimation uuid.
func TestSync_IntimationObserved_SameTxWithUF(t *testing.T) {
	pool := newPool(t)
	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-intim-obs", 0)
	integID := seedIntegration(t, pool, tenantID, acquisition.SourceDJEN)

	parser := &scriptedParser{result: intimationFixture(acquisition.IntimationStatusActive, "")}
	if err := newSyncUCWithParser(pool, parser).OnSyncRequested(
		context.Background(), syncEvent(t, pool, tenantID, integID)); err != nil {
		t.Fatalf("OnSyncRequested() error = %v", err)
	}

	n := countRows(t, pool,
		`SELECT count(*) FROM outbox WHERE type=$1 AND payload->>'tenant_id'=$2`,
		acquisition.TypeIntimationObserved, tenantID)
	if n != 1 {
		t.Fatalf("intimation.observed outbox rows = %d, want 1", n)
	}

	var uf, court, deadline, courtRecordID, caseID, intimationID, aggregateID, aggregateType string
	if err := pool.QueryRow(context.Background(),
		`SELECT payload->>'uf', payload->>'court', payload->>'deadline_start_at',
		        payload->>'court_record_id', payload->>'case_id', payload->>'intimation_id',
		        aggregate_id::text, aggregate_type
		 FROM outbox WHERE type=$1 AND payload->>'tenant_id'=$2`,
		acquisition.TypeIntimationObserved, tenantID).
		Scan(&uf, &court, &deadline, &courtRecordID, &caseID, &intimationID, &aggregateID, &aggregateType); err != nil {
		t.Fatalf("read observed payload: %v", err)
	}

	if court != "TJSP" || uf != "SP" {
		t.Errorf("court/uf = {%q %q}, want {TJSP SP} (uf denormalized via ufFromTribunal)", court, uf)
	}
	if deadline != "2024-01-16" {
		t.Errorf("deadline_start_at = %q, want 2024-01-16", deadline)
	}
	if courtRecordID == "" || caseID == "" {
		t.Errorf("court_record_id/case_id = {%q %q}, want both set", courtRecordID, caseID)
	}
	if aggregateType != "intimation" {
		t.Errorf("aggregate_type = %q, want intimation", aggregateType)
	}
	if intimationID != aggregateID {
		t.Errorf("intimation_id %q != aggregate_id %q", intimationID, aggregateID)
	}
	if _, err := uuid.Parse(aggregateID); err != nil {
		t.Errorf("aggregate_id %q is not a uuid: %v", aggregateID, err)
	}
	// The event carries the real intimation row id.
	var rowID string
	if err := pool.QueryRow(context.Background(),
		`SELECT id::text FROM intimation WHERE tenant_id=$1 AND hash=$2`, tenantID, "intim-evt-1").Scan(&rowID); err != nil {
		t.Fatalf("read intimation id: %v", err)
	}
	if intimationID != rowID {
		t.Errorf("event intimation_id = %q, want the row id %q", intimationID, rowID)
	}
}

// II2: a re-sync bringing data_cancelamento transitions the intimação ACTIVE →
// CANCELLED and commits exactly one acquisition.intimation.cancelled (with the reason)
// in the same tx — and NO second observed for that transition.
func TestSync_IntimationCancelled_OnTransition(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-intim-cancel", 0)
	integID := seedIntegration(t, pool, tenantID, acquisition.SourceDJEN)

	parser := &scriptedParser{result: intimationFixture(acquisition.IntimationStatusActive, "")}
	uc := newSyncUCWithParser(pool, parser)

	// First delivery: the fresh ACTIVE publication → one observed, no cancelled.
	if err := uc.OnSyncRequested(ctx, syncEvent(t, pool, tenantID, integID)); err != nil {
		t.Fatalf("first delivery: %v", err)
	}

	// Second delivery: the SAME intimação re-arrives carrying the retraction.
	parser.result = intimationFixture(acquisition.IntimationStatusCancelled, "retratada pelo tribunal")
	if err := uc.OnSyncRequested(ctx, syncEvent(t, pool, tenantID, integID)); err != nil {
		t.Fatalf("second delivery (cancellation): %v", err)
	}

	// The row flipped to CANCELLED (upsert on conflict).
	var status string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM intimation WHERE tenant_id=$1 AND hash=$2`, tenantID, "intim-evt-1").Scan(&status); err != nil {
		t.Fatalf("read intimation status: %v", err)
	}
	if status != acquisition.IntimationStatusCancelled {
		t.Fatalf("intimation status = %q, want CANCELLED", status)
	}

	// Exactly one cancelled event, carrying the reason.
	cancelled := countRows(t, pool,
		`SELECT count(*) FROM outbox WHERE type=$1 AND payload->>'tenant_id'=$2`,
		acquisition.TypeIntimationCancelled, tenantID)
	if cancelled != 1 {
		t.Fatalf("intimation.cancelled outbox rows = %d, want 1", cancelled)
	}
	var reason, intimationID, aggregateID string
	if err := pool.QueryRow(ctx,
		`SELECT payload->>'reason', payload->>'intimation_id', aggregate_id::text
		 FROM outbox WHERE type=$1 AND payload->>'tenant_id'=$2`,
		acquisition.TypeIntimationCancelled, tenantID).Scan(&reason, &intimationID, &aggregateID); err != nil {
		t.Fatalf("read cancelled payload: %v", err)
	}
	if reason != "retratada pelo tribunal" {
		t.Errorf("reason = %q, want %q", reason, "retratada pelo tribunal")
	}
	if intimationID != aggregateID {
		t.Errorf("intimation_id %q != aggregate_id %q", intimationID, aggregateID)
	}

	// The cancellation is NOT an observed: only the first (ACTIVE) delivery observed it.
	observed := countRows(t, pool,
		`SELECT count(*) FROM outbox WHERE type=$1 AND payload->>'tenant_id'=$2`,
		acquisition.TypeIntimationObserved, tenantID)
	if observed != 1 {
		t.Fatalf("intimation.observed outbox rows = %d, want 1 (the transition emits no new observed)", observed)
	}
}

// II3 — CORRECTNESS CRITICAL: a re-observação of an intimação the user already RESOLVED
// must NOT reset its user_status back to PENDING. The sync upsert flips the DJEN `status`
// (ACTIVE/CANCELLED) but must leave the triagem `user_status` untouched. Sequence:
// (1) first sync lands the intimação PENDING; (2) the user resolves it (RESOLVED);
// (3) a second sync re-arrives with the SAME hash (still ACTIVE) — the upsert runs its
// UPDATE branch; (4) user_status is STILL RESOLVED, and DJEN status STILL ACTIVE.
func TestSync_ReobservationPreservesUserStatus(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-inti-triage", 0)
	integID := seedIntegration(t, pool, tenantID, acquisition.SourceDJEN)

	parser := &scriptedParser{result: intimationFixture(acquisition.IntimationStatusActive, "")}
	syncUC := newSyncUCWithParser(pool, parser)

	// (1) First sync — the intimação lands. Its user_status defaults to PENDING.
	if err := syncUC.OnSyncRequested(ctx, syncEvent(t, pool, tenantID, integID)); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	var intiID, userStatus string
	if err := pool.QueryRow(ctx,
		`SELECT id::text, user_status FROM intimation WHERE tenant_id=$1 AND hash=$2`,
		tenantID, "intim-evt-1").Scan(&intiID, &userStatus); err != nil {
		t.Fatalf("read seeded intimation: %v", err)
	}
	if userStatus != acquisition.IntimationUserStatusPending {
		t.Fatalf("fresh intimation user_status = %q, want PENDING (column default)", userStatus)
	}

	// (2) The user resolves it, through the real write use case (tx + RLS).
	writeUC := acquisition.NewUseCase(
		acquisition.NewRepository(pool), events.NewOutbox(), database.NewUnitOfWork(pool))
	if err := writeUC.ResolveIntimacao(ctx, tenantID, intiID); err != nil {
		t.Fatalf("ResolveIntimacao: %v", err)
	}

	// (3) A second sync brings the SAME hash again (still ACTIVE) → the upsert UPDATE runs.
	parser.result = intimationFixture(acquisition.IntimationStatusActive, "")
	if err := syncUC.OnSyncRequested(ctx, syncEvent(t, pool, tenantID, integID)); err != nil {
		t.Fatalf("re-sync: %v", err)
	}

	// (4) The re-observação did NOT clobber the user's decision: user_status stays RESOLVED,
	// while the DJEN status is still ACTIVE (the upsert touched it, unchanged).
	var afterUserStatus, afterStatus string
	if err := pool.QueryRow(ctx,
		`SELECT user_status, status FROM intimation WHERE tenant_id=$1 AND hash=$2`,
		tenantID, "intim-evt-1").Scan(&afterUserStatus, &afterStatus); err != nil {
		t.Fatalf("read intimation after re-sync: %v", err)
	}
	if afterUserStatus != acquisition.IntimationUserStatusResolved {
		t.Fatalf("user_status after re-sync = %q, want RESOLVED preserved (re-observação must NOT reset triagem)", afterUserStatus)
	}
	if afterStatus != acquisition.IntimationStatusActive {
		t.Fatalf("DJEN status after re-sync = %q, want ACTIVE", afterStatus)
	}
}

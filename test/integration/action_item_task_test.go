//go:build integration

// Fatia 3 (docs/erd-costura-providencia-tarefa-peca.md §2/§6, revisão SÍNCRONA) integration
// test: proves the providência→tarefa creation against a REAL Postgres now that it is
// SYNCHRONOUS — no async hop through the outbox. A confiável providência (declarado, or an IA
// item just Confirmar-ed) mints+links its tarefa INSIDE the same tx via the injected
// TaskCreator (deadline.ActionItemTaskCreator over deadline's sqlc repo): the task row is
// created, action_item.task_id is written and action_item.status flips to CONFIRMED, all
// committed together — and NO actionitem.created / task.created event is emitted for that path.
//
// Each step goes through the REAL use case (real sqlc repos + real transactional outbox + the
// real UnitOfWork), reading the committed rows straight off the tables. The unit-level coverage
// for the individual use cases lives in internal/actionitem/domain_test.go and
// internal/deadline/actionitem_task_test.go; this file is the permanent cross-slice regression
// guard against a real DB.
package integration_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/jusassessoria/platform/internal/actionitem"
	"github.com/jusassessoria/platform/lib/events"
)

// AIT1: the synchronous providência→tarefa path, end to end against a real Postgres.
//
//  1. actionitem.OnIntimationAnalyzed materializes a declarado providência (born confiável) and,
//     in the SAME tx, asks the injected TaskCreator to mint the task and links it back
//     (task_id + status=CONFIRMED). No actionitem.created / task.created event for this path.
//  2. The task row (title = tipo, source = RULE, deadline/intimation/court_record herdados) and
//     the action_item's reverse pointer + status are read back directly with SQL — never through
//     either repo — proving the whole loop committed atomically.
func TestActionItemTask_SynchronousCreation_AgainstRealDB(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)

	// newActionItemUC (action_item_test.go) already injects the real synchronous TaskCreator.
	actionItemUC := newActionItemUC(pool)

	p := seedDeadlineParentsCommitted(ctx, t, pool)
	tenantID := p.tenantID.String()
	intimationID := p.intimationID.String()
	courtRecordID := p.courtRecordID.String()

	// Materialize a declarado (born confiável) providência — its task is created+linked in-tx.
	analyzed := actionitem.IntimationAnalyzed{
		Base:          events.Base{EventID: uuid.NewString()},
		TenantID:      tenantID,
		IntimationID:  intimationID,
		CourtRecordID: courtRecordID,
		Providencias: []actionitem.ProvidenciaCandidate{
			{Tipo: actionitem.TipoContestar, GeraPeca: false, Declarado: true},
		},
	}
	if err := actionItemUC.OnIntimationAnalyzed(ctx, analyzed); err != nil {
		t.Fatalf("OnIntimationAnalyzed() error = %v", err)
	}

	var actionItemID, linkedTaskID, itemStatus string
	if err := pool.QueryRow(ctx,
		`SELECT id::text, task_id::text, status FROM action_item WHERE tenant_id = $1 AND intimation_id = $2 AND tipo = $3`,
		tenantID, intimationID, actionitem.TipoContestar).
		Scan(&actionItemID, &linkedTaskID, &itemStatus); err != nil {
		t.Fatalf("read materialized action_item: %v", err)
	}
	if linkedTaskID == "" {
		t.Fatalf("action_item.task_id is empty, want the linked task id")
	}
	if itemStatus != string(actionitem.StatusConfirmed) {
		t.Errorf("action_item.status = %q, want CONFIRMED (task linked in-tx)", itemStatus)
	}

	// The task row: created with the inherited context and the tipo as its title.
	var taskID, title, status, source, savedCourtRecordID, savedIntimationID string
	var savedActionItemID *string
	if err := pool.QueryRow(ctx, `
		SELECT id::text, title, status, source, court_record_id::text, intimation_id::text, action_item_id::text
		FROM task WHERE action_item_id = $1`, actionItemID).
		Scan(&taskID, &title, &status, &source, &savedCourtRecordID, &savedIntimationID, &savedActionItemID); err != nil {
		t.Fatalf("read created task: %v", err)
	}
	if taskID != linkedTaskID {
		t.Errorf("task id = %q, but action_item.task_id = %q — reverse pointer mismatch", taskID, linkedTaskID)
	}
	if title != actionitem.TipoContestar {
		t.Errorf("task.title = %q, want %q (the tipo)", title, actionitem.TipoContestar)
	}
	if status != "OPEN" || source != "RULE" {
		t.Errorf("task status/source = %q/%q, want OPEN/RULE", status, source)
	}
	if savedCourtRecordID != courtRecordID || savedIntimationID != intimationID {
		t.Errorf("task context ids = cr %q / intim %q, want %q/%q", savedCourtRecordID, savedIntimationID, courtRecordID, intimationID)
	}
	if savedActionItemID == nil || *savedActionItemID != actionItemID {
		t.Errorf("task.action_item_id = %v, want %q", savedActionItemID, actionItemID)
	}

	// The synchronous path emits NO actionitem.created and NO task.created for this action_item.
	if got := countRows(t, pool,
		`SELECT count(*) FROM outbox WHERE type = $1 AND aggregate_id = $2`,
		actionitem.TypeActionItemCreated, actionItemID); got != 0 {
		t.Errorf("actionitem.created rows = %d, want 0 (creation is synchronous)", got)
	}
}

// AIT2: idempotency of the synchronous path against a real Postgres. Re-delivering the SAME
// analysis (a re-Analisar) must NOT mint a second task: the declarado candidate is deduped by
// tipo (ExistsActionItemByTipo), so materializeCandidate short-circuits and the task stays 1:1.
// Even the UNIQUE (action_item_id) on task (migration 0087) would bar a duplicate, but the dedup
// bars it first. The action_item keeps its single linked task + CONFIRMED status throughout.
func TestActionItemTask_RedeliveredAnalysis_NeverDuplicatesTask(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)

	actionItemUC := newActionItemUC(pool)

	p := seedDeadlineParentsCommitted(ctx, t, pool)
	tenantID := p.tenantID.String()
	intimationID := p.intimationID.String()

	analyzed := actionitem.IntimationAnalyzed{
		Base:          events.Base{EventID: uuid.NewString()},
		TenantID:      tenantID,
		IntimationID:  intimationID,
		CourtRecordID: p.courtRecordID.String(),
		Providencias: []actionitem.ProvidenciaCandidate{
			{Tipo: actionitem.TipoContestar, GeraPeca: false, Declarado: true},
		},
	}

	// Deliver the SAME analysis twice (each event a fresh id, so dedup is the per-tipo guard,
	// not the event-id dedup).
	for i := 0; i < 2; i++ {
		analyzed.Base = events.Base{EventID: uuid.NewString()}
		if err := actionItemUC.OnIntimationAnalyzed(ctx, analyzed); err != nil {
			t.Fatalf("OnIntimationAnalyzed() delivery %d error = %v", i, err)
		}
	}

	var actionItemID string
	if err := pool.QueryRow(ctx,
		`SELECT id::text FROM action_item WHERE tenant_id = $1 AND intimation_id = $2 AND tipo = $3`,
		tenantID, intimationID, actionitem.TipoContestar).Scan(&actionItemID); err != nil {
		t.Fatalf("read materialized action_item: %v", err)
	}

	// Exactly one action_item (per-tipo dedup) and exactly one task linked to it.
	if got := countRows(t, pool,
		`SELECT count(*) FROM action_item WHERE tenant_id = $1 AND intimation_id = $2 AND tipo = $3`,
		tenantID, intimationID, actionitem.TipoContestar); got != 1 {
		t.Errorf("action_item rows = %d, want 1 (per-tipo dedup)", got)
	}
	if got := countRows(t, pool,
		`SELECT count(*) FROM task WHERE action_item_id = $1`, actionItemID); got != 1 {
		t.Errorf("task rows for action_item = %d, want 1 (no duplicate on re-analysis)", got)
	}
}

// AIT3: the OTHER entry into the synchronous path — Confirmar. An IA (a_confirmar) item is born
// WITHOUT a task; confirming it mints+links the task in the same tx (status→CONFIRMED,
// task_id set), just like the declarado path, and never emits task.created.
func TestActionItemTask_Confirmar_CreatesTaskSynchronously_AgainstRealDB(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)

	actionItemUC := newActionItemUC(pool)

	p := seedDeadlineParentsCommitted(ctx, t, pool)
	tenantID := p.tenantID.String()
	intimationID := p.intimationID.String()

	analyzed := actionitem.IntimationAnalyzed{
		Base:          events.Base{EventID: uuid.NewString()},
		TenantID:      tenantID,
		IntimationID:  intimationID,
		CourtRecordID: p.courtRecordID.String(),
		Providencias: []actionitem.ProvidenciaCandidate{
			{Tipo: actionitem.TipoManifestar, GeraPeca: false, Declarado: false},
		},
	}
	if err := actionItemUC.OnIntimationAnalyzed(ctx, analyzed); err != nil {
		t.Fatalf("OnIntimationAnalyzed() error = %v", err)
	}

	var actionItemID string
	if err := pool.QueryRow(ctx,
		`SELECT id::text FROM action_item WHERE tenant_id = $1 AND intimation_id = $2 AND tipo = $3`,
		tenantID, intimationID, actionitem.TipoManifestar).Scan(&actionItemID); err != nil {
		t.Fatalf("read materialized action_item: %v", err)
	}

	// Born a_confirmar: no task yet.
	if got := countRows(t, pool,
		`SELECT count(*) FROM task WHERE action_item_id = $1`, actionItemID); got != 0 {
		t.Fatalf("task rows before Confirmar = %d, want 0 (IA item waits)", got)
	}

	confirmed, err := actionItemUC.Confirmar(ctx, tenantID, actionItemID)
	if err != nil {
		t.Fatalf("Confirmar() error = %v", err)
	}
	if confirmed.TipoStatus != actionitem.TipoStatusConfiavel {
		t.Fatalf("Confirmar() tipo_status = %q, want confiavel", confirmed.TipoStatus)
	}

	var linkedTaskID, itemStatus string
	if err := pool.QueryRow(ctx,
		`SELECT task_id::text, status FROM action_item WHERE id = $1`, actionItemID).
		Scan(&linkedTaskID, &itemStatus); err != nil {
		t.Fatalf("read confirmed action_item: %v", err)
	}
	if linkedTaskID == "" {
		t.Errorf("action_item.task_id is empty after Confirmar, want the linked task id")
	}
	if itemStatus != string(actionitem.StatusConfirmed) {
		t.Errorf("action_item.status = %q, want CONFIRMED", itemStatus)
	}
	if got := countRows(t, pool,
		`SELECT count(*) FROM task WHERE action_item_id = $1`, actionItemID); got != 1 {
		t.Errorf("task rows after Confirmar = %d, want 1 (minted in-tx)", got)
	}
}

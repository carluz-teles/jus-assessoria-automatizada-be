//go:build integration

// Fatia 3 (docs/erd-costura-providencia-tarefa-peca.md §2/§6) integration test: proves the
// COMPLETE providência→tarefa loop against a REAL Postgres — actionitem.created materializes,
// deadline's listener creates the task and announces task.created, and actionitem's own
// listener writes the reverse pointer (action_item.task_id) back onto its own table. Each
// step goes through the REAL use case (real sqlc repo + real transactional outbox), reading
// the emitted event straight off the outbox row (as production's asynq listener would decode
// it), never a mock — the unit-level coverage for the individual use cases already lives in
// internal/deadline/actionitem_task_test.go and internal/actionitem/domain_test.go.
package integration_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/jusassessoria/platform/internal/actionitem"
	"github.com/jusassessoria/platform/internal/deadline"
	"github.com/jusassessoria/platform/lib/events"
)

// readOutboxPayload (the (type, aggregate_id) → raw jsonb payload reader) already lives in
// process_activity_generate_test.go — reused here, not redefined.

// AIT1: the full loop, end to end against a real Postgres.
//
//  1. actionitem.OnIntimationAnalyzed materializes a declarado providência (born confiável)
//     and commits actionitem.created to the outbox.
//  2. That REAL outbox payload is decoded into deadline.ActionItemFact (exactly as the asynq
//     listener would) and fed to deadline.OnActionItemCreated, which creates the task
//     (title = tipo, source = RULE, deadline/intimation/court_record herdados) and commits
//     task.created.
//  3. That REAL task.created payload is decoded into actionitem.TaskCreated and fed to
//     actionitem.OnTaskCreated, which writes the reverse pointer.
//  4. action_item.task_id/status and the task row are read back directly with SQL — never
//     through either repo — closing the loop.
func TestActionItemTask_FullLoop_AgainstRealDB(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)

	actionItemUC := newActionItemUC(pool)
	deadlineUC := newDeadlineUC(pool)

	p := seedDeadlineParentsCommitted(ctx, t, pool)
	tenantID := p.tenantID.String()
	intimationID := p.intimationID.String()
	courtRecordID := p.courtRecordID.String()

	// Step 1: materialize a declarado (born confiável) providência.
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

	var actionItemID string
	if err := pool.QueryRow(ctx,
		`SELECT id::text FROM action_item WHERE tenant_id = $1 AND intimation_id = $2 AND tipo = $3`,
		tenantID, intimationID, actionitem.TipoContestar).Scan(&actionItemID); err != nil {
		t.Fatalf("read materialized action_item: %v", err)
	}

	createdPayload := readOutboxPayload(t, pool, actionitem.TypeActionItemCreated, actionItemID)
	var createdEv deadline.ActionItemFact
	if err := json.Unmarshal(createdPayload, &createdEv); err != nil {
		t.Fatalf("unmarshal actionitem.created into deadline.ActionItemFact: %v", err)
	}
	if createdEv.ActionItemID != actionItemID || createdEv.TenantID != tenantID || createdEv.Tipo != actionitem.TipoContestar {
		t.Fatalf("decoded actionitem.created = %+v, unexpected", createdEv)
	}

	// Step 2: deadline's listener reacts, creating the task.
	if err := deadlineUC.OnActionItemCreated(ctx, createdEv); err != nil {
		t.Fatalf("OnActionItemCreated() error = %v", err)
	}

	var taskID, title, status, source, savedCourtRecordID, savedIntimationID string
	var savedActionItemID *string
	if err := pool.QueryRow(ctx, `
		SELECT id::text, title, status, source, court_record_id::text, intimation_id::text, action_item_id::text
		FROM task WHERE action_item_id = $1`, actionItemID).
		Scan(&taskID, &title, &status, &source, &savedCourtRecordID, &savedIntimationID, &savedActionItemID); err != nil {
		t.Fatalf("read created task: %v", err)
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

	taskCreatedPayload := readOutboxPayload(t, pool, deadline.TypeTaskCreated, taskID)
	var taskCreatedEv actionitem.TaskCreated
	if err := json.Unmarshal(taskCreatedPayload, &taskCreatedEv); err != nil {
		t.Fatalf("unmarshal task.created into actionitem.TaskCreated: %v", err)
	}
	if taskCreatedEv.TaskID != taskID || taskCreatedEv.ActionItemID != actionItemID {
		t.Fatalf("decoded task.created = %+v, want task_id=%q action_item_id=%q", taskCreatedEv, taskID, actionItemID)
	}

	// Step 3: actionitem's listener reacts, writing the reverse pointer.
	if err := actionItemUC.OnTaskCreated(ctx, taskCreatedEv); err != nil {
		t.Fatalf("OnTaskCreated() error = %v", err)
	}

	var linkedTaskID, itemStatus string
	if err := pool.QueryRow(ctx,
		`SELECT task_id::text, status FROM action_item WHERE id = $1`, actionItemID).
		Scan(&linkedTaskID, &itemStatus); err != nil {
		t.Fatalf("read linked action_item: %v", err)
	}
	if linkedTaskID != taskID {
		t.Errorf("action_item.task_id = %q, want %q", linkedTaskID, taskID)
	}
	if itemStatus != string(actionitem.StatusConfirmed) {
		t.Errorf("action_item.status = %q, want CONFIRMED", itemStatus)
	}
}

// AIT2: idempotency — redelivering the SAME actionitem.created event never mints a second
// task (the dedup mark), and even if the mark were somehow bypassed, the 0087 UNIQUE
// (action_item_id) would still bar a second row (ErrTaskExistsForActionItem, absorbed as a
// no-op).
func TestActionItemTask_RedeliveredActionItemCreated_NeverDuplicatesTask(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)

	actionItemUC := newActionItemUC(pool)
	deadlineUC := newDeadlineUC(pool)

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
	if err := actionItemUC.OnIntimationAnalyzed(ctx, analyzed); err != nil {
		t.Fatalf("OnIntimationAnalyzed() error = %v", err)
	}

	var actionItemID string
	if err := pool.QueryRow(ctx,
		`SELECT id::text FROM action_item WHERE tenant_id = $1 AND intimation_id = $2 AND tipo = $3`,
		tenantID, intimationID, actionitem.TipoContestar).Scan(&actionItemID); err != nil {
		t.Fatalf("read materialized action_item: %v", err)
	}

	createdPayload := readOutboxPayload(t, pool, actionitem.TypeActionItemCreated, actionItemID)
	var createdEv deadline.ActionItemFact
	if err := json.Unmarshal(createdPayload, &createdEv); err != nil {
		t.Fatalf("unmarshal actionitem.created: %v", err)
	}

	// Deliver the SAME event twice.
	for i := 0; i < 2; i++ {
		if err := deadlineUC.OnActionItemCreated(ctx, createdEv); err != nil {
			t.Fatalf("OnActionItemCreated() delivery %d error = %v", i, err)
		}
	}

	var taskCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM task WHERE action_item_id = $1`, actionItemID).Scan(&taskCount); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if taskCount != 1 {
		t.Errorf("task rows for action_item = %d, want 1 (dedup bars the duplicate)", taskCount)
	}

	var taskCreatedCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM outbox o JOIN task t ON t.id = o.aggregate_id
		WHERE o.type = $1 AND t.action_item_id = $2`,
		deadline.TypeTaskCreated, actionItemID).Scan(&taskCreatedCount); err != nil {
		t.Fatalf("count task.created: %v", err)
	}
	if taskCreatedCount != 1 {
		t.Errorf("task.created rows = %d, want 1 (no phantom re-emit)", taskCreatedCount)
	}
}

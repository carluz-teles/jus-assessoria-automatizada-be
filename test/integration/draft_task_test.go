//go:build integration

// Fatia 4 (docs/erd-costura-providencia-tarefa-peca.md §2/§3, migration 0088) integration
// tests: prove the task-sourced POST /v1/pecas flow against a REAL Postgres, specifically
// the constraint rework that draft_intimation_id_uidx alone could not survive:
//
//   - the ERD §1 central example — 1 intimação with N providências generating N distinct
//     peças — needs N task-sourced drafts for the SAME intimation_id; the new
//     draft_task_id_uidx (scoped to task_id) must not collide with them;
//   - the LEGACY path (no task_id) must still be capped at one draft per (tenant,
//     intimation) — draft_intimation_id_uidx, now scoped to task_id IS NULL, must still
//     enforce it (already proven, unmodified, by TestDraft_Idempotency_SameIntimation in
//     draft_test.go — this file adds the NEW task-sourced side of the same guarantee).
//
// Parent rows are seeded and committed directly via SQL (task/action_item are owned by
// other slices; going through their full use cases is unit-tested elsewhere — see
// action_item_task_test.go for the end-to-end providência→tarefa loop). This file only
// exercises the draft slice's Create use case against real rows.
package integration_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jusassessoria/platform/internal/draft"
)

// seedActionItemWithTask inserts one action_item (gera_peca=true, confiável,
// piece_profile_key=profileKey) linked to intimationID/courtRecordID, plus the task it
// produced (action_item_id set — mirrors the real providência→tarefa loop's end state).
// Returns the task's id.
func seedActionItemWithTask(t *testing.T, pool *pgxpool.Pool, tenantID, intimationID, courtRecordID, tipo, profileKey string) string {
	t.Helper()
	ctx := context.Background()

	var actionItemID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO action_item
			(tenant_id, intimation_id, court_record_id, tipo, gera_peca, piece_profile_key,
			 tipo_origem, tipo_status)
		VALUES ($1, $2, $3, $4, true, $5, 'declarado', 'confiavel')
		RETURNING id::text`,
		tenantID, intimationID, courtRecordID, tipo, profileKey).Scan(&actionItemID); err != nil {
		t.Fatalf("seed action_item: %v", err)
	}

	var taskID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO task
			(tenant_id, court_record_id, intimation_id, title, status, source, action_item_id)
		VALUES ($1, $2, $3, $4, 'OPEN', 'RULE', $5)
		RETURNING id::text`,
		tenantID, courtRecordID, intimationID, tipo, actionItemID).Scan(&taskID); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	if _, err := pool.Exec(ctx,
		`UPDATE action_item SET task_id = $1 WHERE id = $2`, taskID, actionItemID); err != nil {
		t.Fatalf("link action_item.task_id: %v", err)
	}

	return taskID
}

// seedAvulsaTask inserts a manual/avulsa task with NO linked action_item (action_item_id
// IS NULL) — the shape GetActionItemForTask must reject as ErrTaskNotFound.
func seedAvulsaTask(t *testing.T, pool *pgxpool.Pool, tenantID string) string {
	t.Helper()
	var taskID string
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO task (tenant_id, title, status, source)
		VALUES ($1, 'Tarefa avulsa', 'OPEN', 'MANUAL')
		RETURNING id::text`,
		tenantID).Scan(&taskID); err != nil {
		t.Fatalf("seed avulsa task: %v", err)
	}
	return taskID
}

// TestDraft_TaskSourced_TwoTasksSameIntimation_EachGetsOwnDraft is the ERD §1 central
// example proven against the real DB: 1 intimação → 2 providências (contestação +
// impugnação ao valor) → 2 tasks → 2 DISTINCT drafts. Before migration 0088 this would
// 23505 on the second INSERT (draft_intimation_id_uidx had no task_id scoping).
func TestDraft_TaskSourced_TwoTasksSameIntimation_EachGetsOwnDraft(t *testing.T) {
	pool := newPool(t)
	uc := newDraftUC(pool)
	ctx := context.Background()

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-draft-task-multi", 0)
	recordID, caseID := seedCourtRecordCNJ(t, pool, tenantID, "0003333-44.2024.8.26.0099")
	intimationID := seedIntimationTyped(t, pool, tenantID, caseID, recordID, "CITACAO")

	task1 := seedActionItemWithTask(t, pool, tenantID, intimationID, recordID, "contestar", "contestacao")
	task2 := seedActionItemWithTask(t, pool, tenantID, intimationID, recordID, "impugnar_valor", "peticao_inicial")

	result1, err := uc.Create(ctx, draft.CreateCommand{TenantID: tenantID, TaskID: task1})
	if err != nil {
		t.Fatalf("Create (task1): %v", err)
	}
	if !result1.IsNewDraft {
		t.Error("Create (task1): IsNewDraft = false, want true")
	}
	if result1.Draft.PieceType != draft.PieceTypeDefense {
		t.Errorf("Create (task1): PieceType = %q, want %q (contestacao→DEFENSE)", result1.Draft.PieceType, draft.PieceTypeDefense)
	}
	if result1.Draft.CaseID != caseID {
		t.Errorf("Create (task1): CaseID = %q, want %q", result1.Draft.CaseID, caseID)
	}

	result2, err := uc.Create(ctx, draft.CreateCommand{TenantID: tenantID, TaskID: task2})
	if err != nil {
		t.Fatalf("Create (task2): %v", err)
	}
	if !result2.IsNewDraft {
		t.Error("Create (task2): IsNewDraft = false, want true")
	}
	if result2.Draft.PieceType != draft.PieceTypeComplaint {
		t.Errorf("Create (task2): PieceType = %q, want %q (peticao_inicial→COMPLAINT)", result2.Draft.PieceType, draft.PieceTypeComplaint)
	}

	if result1.Draft.ID == result2.Draft.ID {
		t.Fatal("both tasks produced the SAME draft id — draft_task_id_uidx collided across distinct tasks")
	}
	if n := countDrafts(t, pool, tenantID, intimationID); n != 2 {
		t.Errorf("draft count for intimation = %d, want 2 (one per task, same intimation)", n)
	}
}

// TestDraft_TaskSourced_Idempotent_SameTask_ReturnsExistingDraft proves the NEW
// draft_task_id_uidx does its idempotency job: a redelivered/duplicate POST with the
// SAME task_id returns the existing draft (200), never a second row.
func TestDraft_TaskSourced_Idempotent_SameTask_ReturnsExistingDraft(t *testing.T) {
	pool := newPool(t)
	uc := newDraftUC(pool)
	ctx := context.Background()

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-draft-task-idem", 0)
	recordID, caseID := seedCourtRecordCNJ(t, pool, tenantID, "0004444-55.2024.8.26.0099")
	intimationID := seedIntimationTyped(t, pool, tenantID, caseID, recordID, "INTIMACAO")
	taskID := seedActionItemWithTask(t, pool, tenantID, intimationID, recordID, "contestar", "contestacao")

	cmd := draft.CreateCommand{TenantID: tenantID, TaskID: taskID}

	first, err := uc.Create(ctx, cmd)
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}
	if !first.IsNewDraft {
		t.Fatal("first Create: IsNewDraft = false, want true")
	}

	second, err := uc.Create(ctx, cmd)
	if err != nil {
		t.Fatalf("second Create (idempotent): %v", err)
	}
	if second.IsNewDraft {
		t.Error("second Create: IsNewDraft = true, want false (idempotent path)")
	}
	if second.Draft.ID != first.Draft.ID {
		t.Errorf("second Create returned id %q, want the same id %q", second.Draft.ID, first.Draft.ID)
	}
	if n := countDrafts(t, pool, tenantID, intimationID); n != 1 {
		t.Errorf("draft count = %d after two Creates for the same task, want 1 (no duplicate)", n)
	}
}

// TestDraft_TaskSourced_AvulsaTask_ErrTaskNotFound proves an avulsa/manual task (no
// linked action_item) is rejected — it can never produce a task-sourced peça through
// this flow (it has no piece_profile_key to inherit).
func TestDraft_TaskSourced_AvulsaTask_ErrTaskNotFound(t *testing.T) {
	pool := newPool(t)
	uc := newDraftUC(pool)
	ctx := context.Background()

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-draft-task-avulsa", 0)
	taskID := seedAvulsaTask(t, pool, tenantID)

	_, err := uc.Create(ctx, draft.CreateCommand{TenantID: tenantID, TaskID: taskID})
	if err == nil {
		t.Fatal("Create() error = nil, want ErrTaskNotFound")
	}
	if !errors.Is(err, draft.ErrTaskNotFound) {
		t.Errorf("Create() error = %v, want ErrTaskNotFound", err)
	}
}

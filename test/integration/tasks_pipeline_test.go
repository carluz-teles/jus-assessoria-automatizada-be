//go:build integration

// pipeline_stage integration tests (GET /v1/tasks) — prove the LEFT JOIN on the task's VIGENTE
// draft (draft.task_id = task.id AND superseded_at IS NULL — draft_task_id_uidx, migration 0089,
// guarantees at most one such row) plus action_item.gera_peca (via task.action_item_id) end to
// end against a REAL Postgres: pipeline_stage derives correctly per draft state, a superseded
// draft never counts as vigente, ?pipeline=true (TasksQuery.PipelineOnly) narrows the agenda to
// "peça-bound" tasks, and DISMISSED stays excluded regardless of the filter. Parent rows (task/
// action_item/draft) are seeded directly via SQL where they belong to another slice (draft is
// owned by internal/draft — mirrors seedActionItemWithTask in draft_task_test.go); the deadline
// slice's own writes (CreateTask/DismissTask) go through the real use case.
package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jusassessoria/platform/internal/deadline"
)

// seedDraftForTask inserts a draft row bound to taskID directly via SQL (draft is owned by
// another slice). sentAt/filedAt nil mean "not yet reached"; superseded stamps superseded_at so
// the row is NOT the task's vigente draft (draft_task_id_uidx scopes "at most one per task" to
// superseded_at IS NULL).
func seedDraftForTask(t *testing.T, pool *pgxpool.Pool, tenantID, taskID string, sentAt, filedAt *time.Time, superseded bool) string {
	t.Helper()
	var supersededAt *time.Time
	if superseded {
		now := time.Now()
		supersededAt = &now
	}
	var draftID string
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO draft (tenant_id, piece_type, storage_key, task_id, sent_to_signing_at, filed_at, superseded_at)
		VALUES ($1, 'DEFENSE', 'test-storage-key', $2, $3, $4, $5)
		RETURNING id::text`,
		tenantID, taskID, sentAt, filedAt, supersededAt).Scan(&draftID); err != nil {
		t.Fatalf("seed draft for task %s: %v", taskID, err)
	}
	return draftID
}

// findTask locates one task by id in a TasksResult's items — the small, fixed-size results these
// tests read never warrant a map.
func findTask(items []deadline.TaskView, id string) (deadline.TaskView, bool) {
	for _, it := range items {
		if it.ID == id {
			return it, true
		}
	}
	return deadline.TaskView{}, false
}

// TestTasks_PipelineStage_DerivedFromVigenteDraft proves pipeline_stage derives correctly from
// the task's vigente draft: no draft, or a draft not yet sent to signing, is ELABORACAO;
// sent-not-filed is REVISAO; filed is PROTOCOLADO — and a SUPERSEDED draft (even one already sent
// AND filed) is ignored, never counted as vigente.
func TestTasks_PipelineStage_DerivedFromVigenteDraft(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)
	p := seedDeadlineParentsCommitted(ctx, t, pool)
	uc := newDeadlineUC(pool)
	reader := newDeadlineReader(pool)
	tenant := p.tenantID.String()

	mk := func(title string) string {
		task, err := uc.CreateTask(ctx, deadline.CreateTaskCommand{
			TenantID: tenant, UserID: uuid.NewString(), CourtRecordID: p.courtRecordID.String(), Title: title,
		})
		if err != nil {
			t.Fatalf("CreateTask(%q): %v", title, err)
		}
		return task.ID
	}

	noDraft := mk("sem minuta")
	notSent := mk("minuta em elaboração")
	sentNotFiled := mk("minuta em revisão")
	filed := mk("minuta protocolada")
	supersededOnly := mk("minuta reclassificada")

	sentAt := time.Now()
	filedAt := time.Now()
	seedDraftForTask(t, pool, tenant, notSent, nil, nil, false)
	seedDraftForTask(t, pool, tenant, sentNotFiled, &sentAt, nil, false)
	seedDraftForTask(t, pool, tenant, filed, &sentAt, &filedAt, false)
	// A SUPERSEDED draft, already sent AND filed — must still be ignored (not vigente).
	seedDraftForTask(t, pool, tenant, supersededOnly, &sentAt, &filedAt, true)

	res, err := reader.Tasks(ctx, deadline.TasksQuery{
		TenantID: tenant, LastDue: "0001-01-01", LastID: "00000000-0000-0000-0000-000000000000", Limit: 20,
	})
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}

	want := map[string]string{
		noDraft:        deadline.PipelineStageElaboracao,
		notSent:        deadline.PipelineStageElaboracao,
		sentNotFiled:   deadline.PipelineStageRevisao,
		filed:          deadline.PipelineStageProtocolado,
		supersededOnly: deadline.PipelineStageElaboracao,
	}
	for id, wantStage := range want {
		row, ok := findTask(res.Items, id)
		if !ok {
			t.Fatalf("task %s missing from agenda", id)
		}
		if row.PipelineStage != wantStage {
			t.Errorf("task %s pipeline_stage = %q, want %q", id, row.PipelineStage, wantStage)
		}
	}
}

// TestTasks_PipelineFilter_HidesAvulsaTask_KeepsPecaBound proves ?pipeline=true
// (TasksQuery.PipelineOnly) narrows the agenda to "peça-bound" tasks — a vigente draft, OR
// kind='PECA', OR the providência's action_item.gera_peca=true — hiding a plain avulsa task, and
// that DISMISSED stays excluded from the agenda regardless of the filter (a peça-bound task that
// got dispensada never resurfaces, filtered or not).
func TestTasks_PipelineFilter_HidesAvulsaTask_KeepsPecaBound(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)
	p := seedDeadlineParentsCommitted(ctx, t, pool)
	uc := newDeadlineUC(pool)
	reader := newDeadlineReader(pool)
	tenant := p.tenantID.String()

	avulsa, err := uc.CreateTask(ctx, deadline.CreateTaskCommand{
		TenantID: tenant, UserID: uuid.NewString(), CourtRecordID: p.courtRecordID.String(), Title: "tarefa avulsa",
	})
	if err != nil {
		t.Fatalf("CreateTask (avulsa): %v", err)
	}

	kindPeca, err := uc.CreateTask(ctx, deadline.CreateTaskCommand{
		TenantID: tenant, UserID: uuid.NewString(), CourtRecordID: p.courtRecordID.String(),
		Title: "elaborar peça", Kind: "PECA",
	})
	if err != nil {
		t.Fatalf("CreateTask (kind PECA): %v", err)
	}

	geraPecaTaskID := seedActionItemWithTask(t, pool, tenant, p.intimationID.String(), p.courtRecordID.String(), "contestar", "contestacao")

	withDraft, err := uc.CreateTask(ctx, deadline.CreateTaskCommand{
		TenantID: tenant, UserID: uuid.NewString(), CourtRecordID: p.courtRecordID.String(), Title: "com minuta vigente",
	})
	if err != nil {
		t.Fatalf("CreateTask (with draft): %v", err)
	}
	seedDraftForTask(t, pool, tenant, withDraft.ID, nil, nil, false)

	dismissed, err := uc.CreateTask(ctx, deadline.CreateTaskCommand{
		TenantID: tenant, UserID: uuid.NewString(), CourtRecordID: p.courtRecordID.String(),
		Title: "dispensada", Kind: "PECA",
	})
	if err != nil {
		t.Fatalf("CreateTask (to dismiss): %v", err)
	}
	if _, err := uc.DismissTask(ctx, tenant, uuid.NewString(), dismissed.ID); err != nil {
		t.Fatalf("DismissTask: %v", err)
	}

	pipelineRes, err := reader.Tasks(ctx, deadline.TasksQuery{
		TenantID: tenant, PipelineOnly: true,
		LastDue: "0001-01-01", LastID: "00000000-0000-0000-0000-000000000000", Limit: 20,
	})
	if err != nil {
		t.Fatalf("Tasks (pipeline=true): %v", err)
	}
	if _, ok := findTask(pipelineRes.Items, avulsa.ID); ok {
		t.Error("pipeline=true agenda includes the avulsa task, want it hidden")
	}
	for _, id := range []string{kindPeca.ID, geraPecaTaskID, withDraft.ID} {
		if _, ok := findTask(pipelineRes.Items, id); !ok {
			t.Errorf("pipeline=true agenda missing peça-bound task %s", id)
		}
	}
	if _, ok := findTask(pipelineRes.Items, dismissed.ID); ok {
		t.Error("pipeline=true agenda includes a DISMISSED task, want it excluded always")
	}

	unfilteredRes, err := reader.Tasks(ctx, deadline.TasksQuery{
		TenantID: tenant, LastDue: "0001-01-01", LastID: "00000000-0000-0000-0000-000000000000", Limit: 20,
	})
	if err != nil {
		t.Fatalf("Tasks (default, no pipeline filter): %v", err)
	}
	if _, ok := findTask(unfilteredRes.Items, avulsa.ID); !ok {
		t.Error("default agenda (no pipeline filter) missing the avulsa task")
	}
	if _, ok := findTask(unfilteredRes.Items, dismissed.ID); ok {
		t.Error("default agenda includes a DISMISSED task, want it excluded always")
	}
}

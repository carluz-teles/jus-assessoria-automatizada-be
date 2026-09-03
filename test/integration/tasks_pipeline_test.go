//go:build integration

// stage integration tests (GET /v1/tasks) — prove the LEFT JOIN on the task's VIGENTE draft
// (draft.task_id = task.id AND superseded_at IS NULL — draft_task_id_uidx, migration 0089,
// guarantees at most one such row) plus the task's own status end to end against a REAL
// Postgres: the 4-stage stage (A_FAZER/ELABORACAO/REVISAO/CONCLUIDA) derives correctly per
// draft state, a superseded draft never counts as vigente, DONE via MarkTaskDone lands
// CONCLUIDA even with no draft, and DISMISSED stays excluded from the agenda. Parent rows
// (task/draft) are seeded directly via SQL where they belong to another slice (draft is owned
// by internal/draft — mirrors seedActionItemWithTask in draft_task_test.go); the deadline
// slice's own writes (CreateTask/MarkTaskDone/DismissTask) go through the real use case.
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

// TestTasks_Stage_DerivedFromVigenteDraft proves stage derives correctly from the task's own
// status plus its vigente draft: no draft is A_FAZER; a draft not yet sent to signing is
// ELABORACAO; sent-not-filed is REVISAO; filed is CONCLUIDA; a task marked DONE (via
// MarkTaskDone, no draft at all) is CONCLUIDA; and a SUPERSEDED draft (even one already sent AND
// filed) is ignored, never counted as vigente. DISMISSED never surfaces in the agenda regardless.
func TestTasks_Stage_DerivedFromVigenteDraft(t *testing.T) {
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
	doneNoDraft := mk("concluída sem minuta")

	sentAt := time.Now()
	filedAt := time.Now()
	seedDraftForTask(t, pool, tenant, notSent, nil, nil, false)
	seedDraftForTask(t, pool, tenant, sentNotFiled, &sentAt, nil, false)
	seedDraftForTask(t, pool, tenant, filed, &sentAt, &filedAt, false)
	// A SUPERSEDED draft, already sent AND filed — must still be ignored (not vigente).
	seedDraftForTask(t, pool, tenant, supersededOnly, &sentAt, &filedAt, true)

	// A task marked DONE via the real use case, with no draft at all — proves the OR in
	// deriveTaskStage triggers on status alone.
	if _, err := uc.MarkTaskDone(ctx, tenant, uuid.NewString(), doneNoDraft); err != nil {
		t.Fatalf("MarkTaskDone(%q): %v", doneNoDraft, err)
	}

	// A task dismissed via the real use case — must never surface in the agenda.
	dismissed := mk("dispensada")
	if _, err := uc.DismissTask(ctx, tenant, uuid.NewString(), dismissed); err != nil {
		t.Fatalf("DismissTask: %v", err)
	}

	res, err := reader.Tasks(ctx, deadline.TasksQuery{
		TenantID: tenant, LastDue: "0001-01-01", LastID: "00000000-0000-0000-0000-000000000000", Limit: 20,
	})
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}

	want := map[string]string{
		noDraft:        deadline.StageAFazer,
		notSent:        deadline.StageElaboracao,
		sentNotFiled:   deadline.StageRevisao,
		filed:          deadline.StageConcluida,
		supersededOnly: deadline.StageAFazer,
		doneNoDraft:    deadline.StageConcluida,
	}
	for id, wantStage := range want {
		row, ok := findTask(res.Items, id)
		if !ok {
			t.Fatalf("task %s missing from agenda", id)
		}
		if row.Stage != wantStage {
			t.Errorf("task %s stage = %q, want %q", id, row.Stage, wantStage)
		}
	}

	if _, ok := findTask(res.Items, dismissed); ok {
		t.Error("agenda includes a DISMISSED task, want it excluded always")
	}
}

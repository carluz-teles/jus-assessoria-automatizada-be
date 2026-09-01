//go:build integration

// Fatia 5 (docs/erd-costura-providencia-tarefa-peca.md §7 questão 4 — "reclassificação
// depois de gerada a peça") integration tests: prove the COMPLETE reclassify loop against a
// REAL Postgres — actionitem.Reclassificar overrides the providência and announces
// actionitem.reclassified; draft's ReclassifyUseCase reacts by superseding the vigente
// draft; a fresh POST /v1/pecas {task_id} mints the corrected draft; and Create's backfill
// links the OLD draft's superseded_by_draft_id to the NEW one — closing the pointer chain.
// Each step goes through the REAL use case (real sqlc repos + real transactional outbox),
// reading the emitted event straight off the outbox row, never a mock — unit-level coverage
// for the individual use cases already lives in internal/actionitem/domain_test.go and
// internal/draft/reclassify_test.go.
package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jusassessoria/platform/internal/actionitem"
	"github.com/jusassessoria/platform/internal/draft"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// newReclassifyUC wires the real draft reclassify use case over the pool — real sqlc repo,
// real unit-of-work, real dedup. Mirrors worker-ingestao's composition (cmd/worker-ingestao/
// main.go's deadlineMux wiring).
func newReclassifyUC(pool *pgxpool.Pool) *draft.ReclassifyUseCase {
	return draft.NewReclassifyUseCase(
		database.NewUnitOfWork(pool),
		draft.NewRepository(),
		draft.NewReclassifyDeduper(),
	)
}

// actionItemIDForTask resolves a task's linked action_item_id directly with SQL — used to
// drive actionitem.Reclassificar (which takes the providência id, not the task id).
func actionItemIDForTask(t *testing.T, pool *pgxpool.Pool, taskID string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`SELECT action_item_id::text FROM task WHERE id = $1`, taskID).Scan(&id); err != nil {
		t.Fatalf("read action_item_id for task=%s: %v", taskID, err)
	}
	return id
}

// draftSupersedeFields is the subset of draft columns these tests assert on, read back
// directly with SQL (never through the repo) so the assertion is independent of any bug in
// the repo/mapper under test.
type draftSupersedeFields struct {
	supersededAt        *string
	supersededByDraftID *string
	filedAt             *string
}

func readDraftSupersedeFields(t *testing.T, pool *pgxpool.Pool, draftID string) draftSupersedeFields {
	t.Helper()
	var f draftSupersedeFields
	if err := pool.QueryRow(context.Background(), `
		SELECT superseded_at::text, superseded_by_draft_id::text, filed_at::text
		FROM draft WHERE id = $1`, draftID).
		Scan(&f.supersededAt, &f.supersededByDraftID, &f.filedAt); err != nil {
		t.Fatalf("read draft supersede fields id=%s: %v", draftID, err)
	}
	return f
}

// markDraftFiled sets filed_at directly (bypassing the File use case — these tests only
// need the FACT of a filed draft, not the full filing flow).
func markDraftFiled(t *testing.T, pool *pgxpool.Pool, draftID string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE draft SET filed_at = now() WHERE id = $1`, draftID); err != nil {
		t.Fatalf("mark draft filed id=%s: %v", draftID, err)
	}
}

// seedActionItemIA inserts a bare action_item (no task) born tipo_origem='ia' with a
// non-nil confianca score — the ONE shape neither seedActionItemWithTask (action_item_test.go,
// always tipo_origem='declarado', confianca already NULL) nor the mockRepo-backed unit tests
// (domain_test.go) exercise against a REAL Postgres: an ia-classified providência carrying a
// confidence score, about to be overridden to tipo_origem='manual' by Reclassificar.
func seedActionItemIA(t *testing.T, pool *pgxpool.Pool, tenantID, intimationID, courtRecordID, tipo, profileKey string, confianca float64) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO action_item
			(tenant_id, intimation_id, court_record_id, tipo, gera_peca, piece_profile_key,
			 tipo_origem, tipo_status, confianca)
		VALUES ($1, $2, $3, $4, true, $5, 'ia', 'a_confirmar', $6)
		RETURNING id::text`,
		tenantID, intimationID, courtRecordID, tipo, profileKey, confianca).Scan(&id); err != nil {
		t.Fatalf("seed ia action_item: %v", err)
	}
	return id
}

// AR1: the FULL cycle against a real Postgres — reclassificar → old draft superseded →
// create a new draft for the same task → superseded_by_draft_id linked on the old draft.
func TestReclassify_FullCycle_AgainstRealDB(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)

	actionItemUC := newActionItemUC(pool)
	reclassifyUC := newReclassifyUC(pool)
	draftUC := newDraftUC(pool)

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-reclassify-cycle", 0)
	recordID, caseID := seedCourtRecordCNJ(t, pool, tenantID, "0005555-66.2024.8.26.0500")
	intimationID := seedIntimationTyped(t, pool, tenantID, caseID, recordID, "CITACAO")
	taskID := seedActionItemWithTask(t, pool, tenantID, intimationID, recordID, "contestar", "contestacao")
	actionItemID := actionItemIDForTask(t, pool, taskID)

	// Step 1: the FIRST peça for this task.
	first, err := draftUC.Create(ctx, draft.CreateCommand{TenantID: tenantID, TaskID: taskID})
	if err != nil {
		t.Fatalf("Create (first draft): %v", err)
	}
	if !first.IsNewDraft {
		t.Fatal("Create (first draft): IsNewDraft = false, want true")
	}

	// Step 2: reclassify the providência — real UPDATE + real actionitem.reclassified.
	reclassified, err := actionItemUC.Reclassificar(ctx, tenantID, actionItemID, "peticao_inicial", actionitem.TipoManifestar)
	if err != nil {
		t.Fatalf("Reclassificar() error = %v", err)
	}
	if reclassified.PieceProfileKey != "peticao_inicial" || reclassified.Tipo != actionitem.TipoManifestar {
		t.Fatalf("reclassified = %+v, want piece_profile_key=peticao_inicial tipo=manifestar", reclassified)
	}
	if reclassified.TipoOrigem != actionitem.TipoOrigemManual {
		t.Fatalf("reclassified.TipoOrigem = %q, want manual", reclassified.TipoOrigem)
	}

	// Step 3: decode the REAL actionitem.reclassified payload (as the asynq listener would)
	// and feed it to the reclassify use case.
	payload := readOutboxPayload(t, pool, actionitem.TypeActionItemReclassified, actionItemID)
	var ev draft.ActionItemReclassified
	if err := json.Unmarshal(payload, &ev); err != nil {
		t.Fatalf("unmarshal actionitem.reclassified into draft.ActionItemReclassified: %v", err)
	}
	if ev.ActionItemID != actionItemID || ev.TenantID != tenantID {
		t.Fatalf("decoded event = %+v, want action_item_id=%q tenant_id=%q", ev, actionItemID, tenantID)
	}
	if err := reclassifyUC.OnActionItemReclassified(ctx, ev); err != nil {
		t.Fatalf("OnActionItemReclassified() error = %v", err)
	}

	// The OLD draft is now superseded, but not yet linked to a successor.
	oldFields := readDraftSupersedeFields(t, pool, first.Draft.ID)
	if oldFields.supersededAt == nil {
		t.Fatal("old draft superseded_at = nil, want set")
	}
	if oldFields.supersededByDraftID != nil {
		t.Fatalf("old draft superseded_by_draft_id = %v, want nil (no successor yet)", oldFields.supersededByDraftID)
	}

	// Step 4: a fresh POST /v1/pecas {task_id} — the unique index (0089) now allows it,
	// since the old row fell outside its WHERE (superseded_at IS NOT NULL).
	second, err := draftUC.Create(ctx, draft.CreateCommand{TenantID: tenantID, TaskID: taskID})
	if err != nil {
		t.Fatalf("Create (second draft): %v", err)
	}
	if !second.IsNewDraft {
		t.Fatal("Create (second draft): IsNewDraft = false, want true (old draft is superseded, not vigente)")
	}
	if second.Draft.ID == first.Draft.ID {
		t.Fatal("second Create returned the SAME id as the superseded draft, want a distinct new draft")
	}
	if second.Draft.PieceProfileKey != "peticao_inicial" {
		t.Errorf("second draft PieceProfileKey = %q, want peticao_inicial (inherits the RECLASSIFIED providência)", second.Draft.PieceProfileKey)
	}

	// The backfill (Create's populateFromTask path) must have linked old→new in the SAME tx.
	linkedFields := readDraftSupersedeFields(t, pool, first.Draft.ID)
	if linkedFields.supersededByDraftID == nil || *linkedFields.supersededByDraftID != second.Draft.ID {
		t.Fatalf("old draft superseded_by_draft_id = %v, want %q", linkedFields.supersededByDraftID, second.Draft.ID)
	}

	// GetDraftByTaskID (the idempotent-fetch path) must resolve to the NEW vigente draft,
	// never the superseded one: a third Create call for the same task_id hits the unique
	// index again (second.Draft is still vigente) and fetches back through it.
	third, err := draftUC.Create(ctx, draft.CreateCommand{TenantID: tenantID, TaskID: taskID})
	if err != nil {
		t.Fatalf("Create (third, idempotent): %v", err)
	}
	if third.IsNewDraft {
		t.Fatal("Create (third): IsNewDraft = true, want false (second draft is still vigente)")
	}
	if third.Draft.ID != second.Draft.ID {
		t.Errorf("GetDraftByTaskID (via idempotent Create) = %q, want %q (the vigente draft)", third.Draft.ID, second.Draft.ID)
	}
}

// AR2: HasFiledDraftForActionItem blocks reclassification once the providência's peça has
// been protocolada — no UPDATE happens, the action_item row stays exactly as it was.
func TestReclassify_FiledDraft_BlocksReclassification(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)

	actionItemUC := newActionItemUC(pool)
	draftUC := newDraftUC(pool)

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-reclassify-filed", 0)
	recordID, caseID := seedCourtRecordCNJ(t, pool, tenantID, "0005555-66.2024.8.26.0600")
	intimationID := seedIntimationTyped(t, pool, tenantID, caseID, recordID, "CITACAO")
	taskID := seedActionItemWithTask(t, pool, tenantID, intimationID, recordID, "contestar", "contestacao")
	actionItemID := actionItemIDForTask(t, pool, taskID)

	created, err := draftUC.Create(ctx, draft.CreateCommand{TenantID: tenantID, TaskID: taskID})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	markDraftFiled(t, pool, created.Draft.ID)

	_, err = actionItemUC.Reclassificar(ctx, tenantID, actionItemID, "peticao_inicial", actionitem.TipoManifestar)
	if err == nil {
		t.Fatal("Reclassificar() error = nil, want ErrActionItemHasFiledDraft")
	}
	if !errors.Is(err, actionitem.ErrActionItemHasFiledDraft) {
		t.Fatalf("Reclassificar() error = %v, want ErrActionItemHasFiledDraft", err)
	}

	var tipo, pieceProfileKey string
	if err := pool.QueryRow(ctx,
		`SELECT tipo, piece_profile_key FROM action_item WHERE id = $1`, actionItemID).
		Scan(&tipo, &pieceProfileKey); err != nil {
		t.Fatalf("read action_item: %v", err)
	}
	if tipo != "contestar" || pieceProfileKey != "contestacao" {
		t.Errorf("action_item after blocked reclassify = {%q %q}, want unchanged {contestar contestacao}", tipo, pieceProfileKey)
	}
}

// AR3: idempotency — reclassifying with the SAME (piece_profile_key, tipo) twice, once
// already manual, is a no-op the second time: no second actionitem.reclassified row.
func TestReclassify_Idempotent_SameValuesTwice(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)

	actionItemUC := newActionItemUC(pool)

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-reclassify-idem", 0)
	recordID, caseID := seedCourtRecordCNJ(t, pool, tenantID, "0005555-66.2024.8.26.0700")
	intimationID := seedIntimationTyped(t, pool, tenantID, caseID, recordID, "CITACAO")
	taskID := seedActionItemWithTask(t, pool, tenantID, intimationID, recordID, "contestar", "contestacao")
	actionItemID := actionItemIDForTask(t, pool, taskID)

	if _, err := actionItemUC.Reclassificar(ctx, tenantID, actionItemID, "apelacao", actionitem.TipoRecorrer); err != nil {
		t.Fatalf("Reclassificar() first call error = %v", err)
	}
	if _, err := actionItemUC.Reclassificar(ctx, tenantID, actionItemID, "apelacao", actionitem.TipoRecorrer); err != nil {
		t.Fatalf("Reclassificar() second (idempotent) call error = %v", err)
	}

	if got := countRows(t, pool,
		`SELECT count(*) FROM outbox WHERE type = $1 AND aggregate_id = $2`,
		actionitem.TypeActionItemReclassified, actionItemID); got != 1 {
		t.Errorf("actionitem.reclassified rows = %d, want 1 (second call must not re-emit)", got)
	}
}

// AR4: the listener's OWN race defense — the SQL guard (superseded_at IS NULL AND
// filed_at IS NULL) makes a second delivery over an already-superseded draft a silent
// no-op, and a FILED draft is NEVER retroactively superseded even if the event still
// arrives (e.g. a race between File and a reclassify already in flight).
func TestReclassify_Listener_RaceDefense(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)

	draftUC := newDraftUC(pool)
	reclassifyUC := newReclassifyUC(pool)

	t.Run("already superseded draft is untouched by a second delivery", func(t *testing.T) {
		tenantID := uuid.NewString()
		seedTenant(t, pool, tenantID, "org-reclassify-race-a", 0)
		recordID, caseID := seedCourtRecordCNJ(t, pool, tenantID, "0005555-66.2024.8.26.0800")
		intimationID := seedIntimationTyped(t, pool, tenantID, caseID, recordID, "CITACAO")
		taskID := seedActionItemWithTask(t, pool, tenantID, intimationID, recordID, "contestar", "contestacao")
		actionItemID := actionItemIDForTask(t, pool, taskID)

		created, err := draftUC.Create(ctx, draft.CreateCommand{TenantID: tenantID, TaskID: taskID})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}

		ev := draft.ActionItemReclassified{Base: events.Base{EventID: uuid.NewString()}, TenantID: tenantID, ActionItemID: actionItemID}
		if err := reclassifyUC.OnActionItemReclassified(ctx, ev); err != nil {
			t.Fatalf("OnActionItemReclassified() first call error = %v", err)
		}
		first := readDraftSupersedeFields(t, pool, created.Draft.ID)
		if first.supersededAt == nil {
			t.Fatal("draft superseded_at = nil after first delivery, want set")
		}

		// A second, replayed-past-dedup delivery must be a pure no-op: superseded_at stays
		// EXACTLY as the first call set it (the SQL WHERE superseded_at IS NULL never
		// matches again).
		ev2 := draft.ActionItemReclassified{Base: events.Base{EventID: uuid.NewString()}, TenantID: tenantID, ActionItemID: actionItemID}
		if err := reclassifyUC.OnActionItemReclassified(ctx, ev2); err != nil {
			t.Fatalf("OnActionItemReclassified() second call error = %v", err)
		}
		second := readDraftSupersedeFields(t, pool, created.Draft.ID)
		if second.supersededAt == nil || *second.supersededAt != *first.supersededAt {
			t.Errorf("superseded_at changed on second delivery: first=%v second=%v, want unchanged", *first.supersededAt, second.supersededAt)
		}
	})

	t.Run("filed draft is never superseded", func(t *testing.T) {
		tenantID := uuid.NewString()
		seedTenant(t, pool, tenantID, "org-reclassify-race-b", 0)
		recordID, caseID := seedCourtRecordCNJ(t, pool, tenantID, "0005555-66.2024.8.26.0900")
		intimationID := seedIntimationTyped(t, pool, tenantID, caseID, recordID, "CITACAO")
		taskID := seedActionItemWithTask(t, pool, tenantID, intimationID, recordID, "contestar", "contestacao")
		actionItemID := actionItemIDForTask(t, pool, taskID)

		created, err := draftUC.Create(ctx, draft.CreateCommand{TenantID: tenantID, TaskID: taskID})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		markDraftFiled(t, pool, created.Draft.ID)

		ev := draft.ActionItemReclassified{Base: events.Base{EventID: uuid.NewString()}, TenantID: tenantID, ActionItemID: actionItemID}
		if err := reclassifyUC.OnActionItemReclassified(ctx, ev); err != nil {
			t.Fatalf("OnActionItemReclassified() error = %v", err)
		}

		fields := readDraftSupersedeFields(t, pool, created.Draft.ID)
		if fields.supersededAt != nil {
			t.Errorf("filed draft superseded_at = %v, want nil (a FILED draft is frozen, never superseded)", *fields.supersededAt)
		}
	})
}

// AR5: the QA gap this file was missing — reclassifying an action_item that is
// tipo_origem='ia' with a non-nil confianca (e.g. 0.8) into tipo_origem='manual' must reset
// confianca to NULL in the SAME UPDATE, against a REAL Postgres. Migration 0086's
// action_item_check1 (CHECK (tipo_origem = 'ia' OR confianca IS NULL)) rejects the row the
// instant tipo_origem flips away from 'ia' while the old score lingers — this is exactly the
// scenario the QA's throwaway test proved (positive+negative) and then deleted: neither the
// unit test's mock (which resets Confianca unconditionally, proving nothing about the real
// CHECK) nor seedActionItemWithTask (always tipo_origem='declarado', confianca already NULL)
// exercises the real reset. This test does, reading the row back with direct SQL — never
// through the repo/mapper under test — so the assertion is independent of any bug there.
func TestReclassify_IAOrigemWithConfianca_ResetsConfiancaAndSatisfiesCheck(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)
	actionItemUC := newActionItemUC(pool)

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-reclassify-ia-confianca", 0)
	recordID, caseID := seedCourtRecordCNJ(t, pool, tenantID, "0005555-66.2024.8.26.1000")
	intimationID := seedIntimationTyped(t, pool, tenantID, caseID, recordID, "CITACAO")
	actionItemID := seedActionItemIA(t, pool, tenantID, intimationID, recordID, "manifestar", "contestacao", 0.8)

	reclassified, err := actionItemUC.Reclassificar(ctx, tenantID, actionItemID, "peticao_inicial", actionitem.TipoContestar)
	if err != nil {
		t.Fatalf("Reclassificar() error = %v, want nil (the UPDATE must reset confianca to satisfy action_item_check1)", err)
	}
	if reclassified.TipoOrigem != actionitem.TipoOrigemManual {
		t.Fatalf("reclassified.TipoOrigem = %q, want manual", reclassified.TipoOrigem)
	}
	if reclassified.Confianca != nil {
		t.Errorf("reclassified.Confianca = %v, want nil (reset)", *reclassified.Confianca)
	}

	// Read back straight from the table, bypassing the repo/mapper: confianca is NULL on
	// disk, not just on the returned entity.
	var tipoOrigemOnDisk string
	var confiancaOnDisk *float64
	if err := pool.QueryRow(ctx,
		`SELECT tipo_origem, confianca FROM action_item WHERE id = $1`, actionItemID).
		Scan(&tipoOrigemOnDisk, &confiancaOnDisk); err != nil {
		t.Fatalf("read action_item: %v", err)
	}
	if tipoOrigemOnDisk != "manual" {
		t.Fatalf("tipo_origem on disk = %q, want manual", tipoOrigemOnDisk)
	}
	if confiancaOnDisk != nil {
		t.Errorf("confianca on disk = %v, want NULL (tipo_origem=manual + confianca NOT NULL violates action_item_check1)", *confiancaOnDisk)
	}
}

// AR6: the negative half — proven directly with raw SQL, independent of any application
// code, that action_item_check1 really exists and really blocks the un-reset case. An UPDATE
// that flips tipo_origem to 'manual' WITHOUT touching confianca must be rejected by Postgres.
// This is why ReclassifyActionItem's own UPDATE (queries/actionitem.sql) has to set
// confianca = NULL in the same statement: remove that clause and THIS test (unchanged) starts
// failing with the exact same error the raw SQL below proves, while AR5 above would start
// failing at the use-case boundary instead.
func TestActionItemCheck1_RejectsManualOrigemWithConfianca(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-actionitem-check1", 0)
	recordID, caseID := seedCourtRecordCNJ(t, pool, tenantID, "0005555-66.2024.8.26.1100")
	intimationID := seedIntimationTyped(t, pool, tenantID, caseID, recordID, "CITACAO")
	actionItemID := seedActionItemIA(t, pool, tenantID, intimationID, recordID, "manifestar", "contestacao", 0.8)

	_, err := pool.Exec(ctx,
		`UPDATE action_item SET tipo_origem = 'manual' WHERE id = $1`, actionItemID)
	if err == nil {
		t.Fatal("UPDATE tipo_origem='manual' without resetting confianca succeeded, want a CHECK violation (action_item_check1)")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("error = %v (%T), want a *pgconn.PgError", err, err)
	}
	if pgErr.ConstraintName != "action_item_check1" {
		t.Errorf("violated constraint = %q, want action_item_check1", pgErr.ConstraintName)
	}
}

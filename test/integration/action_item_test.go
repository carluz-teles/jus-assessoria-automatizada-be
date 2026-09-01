//go:build integration

// Action item (Providência) integration tests — prove the motor de precedência
// (tipo_origem/tipo_status, docs/erd-costura-providencia-tarefa-peca.md §3) and the guard
// aditivo (OnIntimationAnalyzed's re-analysis semantics) against a REAL Postgres, using the
// real actionitem.NewRepository() (sqlc) over a real pool/tx — never mockRepo. mockRepo-backed
// coverage already lives in internal/actionitem/domain_test.go; this file is the permanent
// regression guard the Reviewer asked for after the QA's proving test was written as a
// throwaway and deleted (CHANGES_REQUIRED).
package integration_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jusassessoria/platform/internal/actionitem"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// newActionItemUC wires the real use case over the pool: sqlc repo, transactional outbox,
// stateless dedup and the unit of work — exactly the worker's composition (mirrors
// newDeadlineUC in deadline_listener_test.go).
func newActionItemUC(pool *pgxpool.Pool) *actionitem.UseCase {
	return actionitem.NewUseCase(
		actionitem.NewRepository(),
		events.NewOutbox(),
		actionitem.NewDedup(),
		database.NewUnitOfWork(pool),
	)
}

// actionItemRow is the subset of action_item columns these tests assert on, read back
// directly with SQL (never through the repo) so the assertion is independent of any bug in
// the repo/mapper under test.
type actionItemRow struct {
	id         string
	tipoOrigem string
	tipoStatus string
	status     string
	confianca  *float64
}

// readActionItemByTipo reads the one action_item row for (tenantID, intimationID, tipo). It
// fails the test if there is not exactly one match — every scenario below is built so a
// given tipo identifies a single row at read time.
func readActionItemByTipo(t *testing.T, pool *pgxpool.Pool, tenantID, intimationID, tipo string) actionItemRow {
	t.Helper()
	var row actionItemRow
	if err := pool.QueryRow(context.Background(), `
		SELECT id::text, tipo_origem, tipo_status, status, confianca
		FROM action_item
		WHERE tenant_id = $1 AND intimation_id = $2 AND tipo = $3`,
		tenantID, intimationID, tipo).
		Scan(&row.id, &row.tipoOrigem, &row.tipoStatus, &row.status, &row.confianca); err != nil {
		t.Fatalf("read action_item tipo=%q: %v", tipo, err)
	}
	return row
}

// AI1: OnIntimationAnalyzed against a real Postgres materializes one row per candidate and
// derives tipo_origem/tipo_status through the REAL motor de precedência: a declarado
// candidate is born declarado/confiável, an IA-ambíguo one is born ia/a_confirmar — read back
// straight from the action_item table, not through the repo's own Get.
func TestActionItem_OnIntimationAnalyzed_DerivesPrecedenceAgainstRealDB(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)
	uc := newActionItemUC(pool)

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-actionitem-precedence", 0)
	recordID, caseID := seedCourtRecordCNJ(t, pool, tenantID, "0002222-33.2023.8.26.0100")
	intimationID := seedIntimationReturningID(t, pool, tenantID, caseID, recordID)

	confianca := 0.65
	ev := actionitem.IntimationAnalyzed{
		Base:          events.Base{EventID: uuid.NewString()},
		TenantID:      tenantID,
		IntimationID:  intimationID,
		CourtRecordID: recordID,
		Providencias: []actionitem.ProvidenciaCandidate{
			{Tipo: actionitem.TipoContestar, GeraPeca: false, Declarado: true},
			{Tipo: actionitem.TipoManifestar, GeraPeca: false, Declarado: false, Confianca: &confianca},
		},
	}
	if err := uc.OnIntimationAnalyzed(ctx, ev); err != nil {
		t.Fatalf("OnIntimationAnalyzed() error = %v", err)
	}

	declarado := readActionItemByTipo(t, pool, tenantID, intimationID, actionitem.TipoContestar)
	if declarado.tipoOrigem != string(actionitem.TipoOrigemDeclarado) {
		t.Errorf("declarado.tipo_origem = %q, want %q", declarado.tipoOrigem, actionitem.TipoOrigemDeclarado)
	}
	if declarado.tipoStatus != string(actionitem.TipoStatusConfiavel) {
		t.Errorf("declarado.tipo_status = %q, want %q", declarado.tipoStatus, actionitem.TipoStatusConfiavel)
	}
	if declarado.status != string(actionitem.StatusSuggested) {
		t.Errorf("declarado.status = %q, want SUGGESTED", declarado.status)
	}
	if declarado.confianca != nil {
		t.Errorf("declarado.confianca = %v, want NULL (only ia-derived items carry a score)", *declarado.confianca)
	}

	inferido := readActionItemByTipo(t, pool, tenantID, intimationID, actionitem.TipoManifestar)
	if inferido.tipoOrigem != string(actionitem.TipoOrigemIA) {
		t.Errorf("inferido.tipo_origem = %q, want %q", inferido.tipoOrigem, actionitem.TipoOrigemIA)
	}
	if inferido.tipoStatus != string(actionitem.TipoStatusAConfirmar) {
		t.Errorf("inferido.tipo_status = %q, want %q", inferido.tipoStatus, actionitem.TipoStatusAConfirmar)
	}
	if inferido.confianca == nil || *inferido.confianca != confianca {
		t.Errorf("inferido.confianca = %v, want %v", inferido.confianca, confianca)
	}

	// Only the confiável (declarado) item is born ready — it alone emits actionitem.created,
	// committed for real in the outbox table in the same tx as the two inserts above.
	if got := countRows(t, pool,
		`SELECT count(*) FROM outbox WHERE type = $1 AND aggregate_id = $2`,
		actionitem.TypeActionItemCreated, declarado.id); got != 1 {
		t.Errorf("actionitem.created rows for declarado item = %d, want 1", got)
	}
	if got := countRows(t, pool,
		`SELECT count(*) FROM outbox WHERE type = $1 AND aggregate_id = $2`,
		actionitem.TypeActionItemCreated, inferido.id); got != 0 {
		t.Errorf("actionitem.created rows for a_confirmar item = %d, want 0 (not born ready)", got)
	}
}

// AI2: the guard aditivo against a real Postgres. First analysis materializes three
// candidates: one declarado (born confiável), and two IA-ambíguos (born a_confirmar). One of
// the a_confirmar items is then really CONFIRMED via uc.Confirmar (tipo_status →
// confiável) — the row this test must find intact after the re-analysis below, same id, same
// tipo_status. A second OnIntimationAnalyzed delivery (re-Analisar) re-proposes the declarado
// candidate (must be skipped, not duplicated — ExistsActionItemByTipo) and proposes ONE new
// IA candidate. It must: (a) leave the declarado item untouched; (b) leave the confirmed item
// untouched (same id, tipo_status still confiável) even though it is one of the two "1 dos 2"
// items from the first run; (c) delete the OTHER first-run item, which was still
// SUGGESTED+a_confirmar+task_id-NULL, and replace it with the fresh candidate.
func TestActionItem_OnIntimationAnalyzed_GuardAditivoAgainstRealDB(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)
	uc := newActionItemUC(pool)

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-actionitem-guard", 0)
	recordID, caseID := seedCourtRecordCNJ(t, pool, tenantID, "0002222-33.2023.8.26.0200")
	intimationID := seedIntimationReturningID(t, pool, tenantID, caseID, recordID)

	run1 := actionitem.IntimationAnalyzed{
		Base:          events.Base{EventID: uuid.NewString()},
		TenantID:      tenantID,
		IntimationID:  intimationID,
		CourtRecordID: recordID,
		Providencias: []actionitem.ProvidenciaCandidate{
			{Tipo: actionitem.TipoContestar, GeraPeca: false, Declarado: true},
			{Tipo: actionitem.TipoManifestar, GeraPeca: false, Declarado: false},
			{Tipo: actionitem.TipoCumprir, GeraPeca: false, Declarado: false},
		},
	}
	if err := uc.OnIntimationAnalyzed(ctx, run1); err != nil {
		t.Fatalf("OnIntimationAnalyzed(run1) error = %v", err)
	}

	declaradoBefore := readActionItemByTipo(t, pool, tenantID, intimationID, actionitem.TipoContestar)
	confirmedBefore := readActionItemByTipo(t, pool, tenantID, intimationID, actionitem.TipoManifestar)
	replaceableBefore := readActionItemByTipo(t, pool, tenantID, intimationID, actionitem.TipoCumprir)
	if confirmedBefore.tipoStatus != string(actionitem.TipoStatusAConfirmar) {
		t.Fatalf("confirmedBefore.tipo_status = %q, want a_confirmar (precondition)", confirmedBefore.tipoStatus)
	}

	// Really confirm ONE of the two a_confirmar items (the real HTTP-facing use case, not a
	// direct UPDATE) — "1 dos 2 already CONFIRMED" before the re-analysis.
	confirmed, err := uc.Confirmar(ctx, tenantID, confirmedBefore.id)
	if err != nil {
		t.Fatalf("Confirmar() error = %v", err)
	}
	if confirmed.TipoStatus != actionitem.TipoStatusConfiavel {
		t.Fatalf("Confirmar() tipo_status = %q, want confiavel", confirmed.TipoStatus)
	}

	// Re-Analisar: repeats the declarado candidate (must dedup, not duplicate) and proposes a
	// brand-new IA candidate (tipo=recorrer) neither prior event carried.
	run2 := actionitem.IntimationAnalyzed{
		Base:          events.Base{EventID: uuid.NewString()},
		TenantID:      tenantID,
		IntimationID:  intimationID,
		CourtRecordID: recordID,
		Providencias: []actionitem.ProvidenciaCandidate{
			{Tipo: actionitem.TipoContestar, GeraPeca: false, Declarado: true},
			{Tipo: actionitem.TipoRecorrer, GeraPeca: false, Declarado: false},
		},
	}
	if err := uc.OnIntimationAnalyzed(ctx, run2); err != nil {
		t.Fatalf("OnIntimationAnalyzed(run2) error = %v", err)
	}

	// (a) declarado item untouched: same id, same tipo_status.
	declaradoAfter := readActionItemByTipo(t, pool, tenantID, intimationID, actionitem.TipoContestar)
	if declaradoAfter.id != declaradoBefore.id {
		t.Errorf("declarado id changed: before=%s after=%s, want unchanged", declaradoBefore.id, declaradoAfter.id)
	}
	if declaradoAfter.tipoStatus != string(actionitem.TipoStatusConfiavel) {
		t.Errorf("declarado.tipo_status after re-analysis = %q, want confiavel", declaradoAfter.tipoStatus)
	}

	// (b) the CONFIRMED item survives intact: same id, same tipo_status (confiável).
	confirmedAfter := readActionItemByTipo(t, pool, tenantID, intimationID, actionitem.TipoManifestar)
	if confirmedAfter.id != confirmedBefore.id {
		t.Errorf("confirmed id changed: before=%s after=%s, want unchanged (guard aditivo must never touch it)", confirmedBefore.id, confirmedAfter.id)
	}
	if confirmedAfter.tipoStatus != string(actionitem.TipoStatusConfiavel) {
		t.Errorf("confirmed.tipo_status after re-analysis = %q, want confiavel (unchanged)", confirmedAfter.tipoStatus)
	}

	// (c) the OTHER item — still SUGGESTED+a_confirmar+task_id-NULL — was deleted by the
	// guard's replaceable-subset DELETE.
	if got := countRows(t, pool,
		`SELECT count(*) FROM action_item WHERE id = $1`, replaceableBefore.id); got != 0 {
		t.Errorf("replaceable item (id=%s) rows after re-analysis = %d, want 0 (must be replaced)", replaceableBefore.id, got)
	}

	// ...and the fresh IA candidate from run2 was inserted in its place, born a_confirmar.
	fresh := readActionItemByTipo(t, pool, tenantID, intimationID, actionitem.TipoRecorrer)
	if fresh.tipoStatus != string(actionitem.TipoStatusAConfirmar) {
		t.Errorf("fresh candidate tipo_status = %q, want a_confirmar", fresh.tipoStatus)
	}

	// Exactly 3 rows survive for this intimação: declarado, confirmed, fresh — no duplicate
	// from the repeated declarado candidate, and the replaced item is gone.
	if got := countRows(t, pool,
		`SELECT count(*) FROM action_item WHERE tenant_id = $1 AND intimation_id = $2`,
		tenantID, intimationID); got != 3 {
		t.Errorf("action_item rows for intimation = %d, want 3", got)
	}
}

// AI3: RLS barrier 2 (docs §4d.4) actually applies to action_item, not just its sibling
// tables — one tenant's providências are invisible under another tenant's app.tenant_id
// scope, and invisible entirely with no scope set. Mirrors TestRLS_TenantIsolation
// (rls_test.go), scoped to action_item instead of app_user.
func TestActionItem_RLS_TenantIsolation(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)
	uc := newActionItemUC(pool)

	// Idempotent — safe even if TestRLS_TenantIsolation already created the role/grants in
	// this run; ALL TABLES IN SCHEMA public already includes action_item regardless of test
	// order, since migrations run once in TestMain before any test executes.
	mustExec(t, pool, `DO $$ BEGIN
		IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_rls') THEN
			CREATE ROLE app_rls;
		END IF;
	END $$`)
	mustExec(t, pool, `GRANT USAGE ON SCHEMA public TO app_rls`)
	mustExec(t, pool, `GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO app_rls`)

	tenantA := uuid.NewString()
	tenantB := uuid.NewString()
	seedTenant(t, pool, tenantA, "org-actionitem-rls-a", 0)
	seedTenant(t, pool, tenantB, "org-actionitem-rls-b", 0)

	recordA, caseA := seedCourtRecordCNJ(t, pool, tenantA, "0002222-33.2023.8.26.0300")
	intimationA := seedIntimationReturningID(t, pool, tenantA, caseA, recordA)
	evA := actionitem.IntimationAnalyzed{
		Base:          events.Base{EventID: uuid.NewString()},
		TenantID:      tenantA,
		IntimationID:  intimationA,
		CourtRecordID: recordA,
		Providencias:  []actionitem.ProvidenciaCandidate{{Tipo: actionitem.TipoContestar, GeraPeca: false, Declarado: true}},
	}
	if err := uc.OnIntimationAnalyzed(ctx, evA); err != nil {
		t.Fatalf("OnIntimationAnalyzed(tenantA) error = %v", err)
	}

	recordB, caseB := seedCourtRecordCNJ(t, pool, tenantB, "0002222-33.2023.8.26.0400")
	intimationB := seedIntimationReturningID(t, pool, tenantB, caseB, recordB)
	evB := actionitem.IntimationAnalyzed{
		Base:          events.Base{EventID: uuid.NewString()},
		TenantID:      tenantB,
		IntimationID:  intimationB,
		CourtRecordID: recordB,
		Providencias: []actionitem.ProvidenciaCandidate{
			{Tipo: actionitem.TipoManifestar, GeraPeca: false, Declarado: true},
			{Tipo: actionitem.TipoCumprir, GeraPeca: false, Declarado: true},
		},
	}
	if err := uc.OnIntimationAnalyzed(ctx, evB); err != nil {
		t.Fatalf("OnIntimationAnalyzed(tenantB) error = %v", err)
	}

	tests := []struct {
		name     string
		tenantID string // empty = do not set app.tenant_id at all
		want     int
	}{
		{name: "tenant A sees only its own action_item rows", tenantID: tenantA, want: 1},
		{name: "tenant B sees only its own action_item rows", tenantID: tenantB, want: 2},
		{name: "no tenant set sees nothing", tenantID: "", want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := countActionItemsAsRLSRole(t, tt.tenantID)
			if got != tt.want {
				t.Errorf("action_item count under RLS = %d, want %d", got, tt.want)
			}
		})
	}
}

// countActionItemsAsRLSRole mirrors countAppUsersAsRLSRole (rls_test.go) exactly, scoped to
// action_item: a dedicated connection (never a pooled one — see that function's doc for why
// a fresh custom-GUC session matters for the "no tenant set" case) dropped to the non-owner
// app_rls role, with app.tenant_id set via the same set_config the UnitOfWork issues in
// production.
func countActionItemsAsRLSRole(t *testing.T, tenantID string) int {
	t.Helper()
	ctx := context.Background()

	conn, err := pgx.Connect(ctx, connString)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)

	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) // read-only probe; never commit

	if _, err := tx.Exec(ctx, "SET LOCAL ROLE app_rls"); err != nil {
		t.Fatalf("set role: %v", err)
	}
	if tenantID != "" {
		if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
			t.Fatalf("set_config: %v", err)
		}
	}

	var count int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM action_item").Scan(&count); err != nil {
		t.Fatalf("count action_item: %v", err)
	}
	return count
}

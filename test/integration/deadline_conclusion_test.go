//go:build integration

// Achado 2 (deadline lifecycle reconciliation) — end-to-end against a REAL Postgres with
// every migration applied:
//   - fatia 2b: OnCourtRecordArchived resolves every PENDING/OPEN/MISSED prazo of an
//     archived court_record to RESOLVED_ON_CONCLUSION (migration 0098), auditing one
//     deadline_event per row and emitting one deadline.resolved_on_conclusion each; a MET
//     prazo is left untouched;
//   - fatia 2a: the enrichment merge path (a DJEN placeholder graduating into a
//     pre-existing graded record) repoints the placeholder's deadlines the SAME way it
//     already repoints intimations, and announces acquisition.court_record_superseded;
//   - fatia 2c.1: the notifications in-app consumer materializes a low-priority
//     ("severidade='info'", the migration 0097 DB default) aviso for
//     deadline.resolved_on_conclusion;
//   - fatia 2c.2: the intimações "fila ativa" read model (ListIntimacoes/
//     SummarizeIntimacoes) excludes a PENDING-de-triagem intimação whose court_record is
//     already ARCHIVED.
package integration_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jusassessoria/platform/internal/acquisition"
	"github.com/jusassessoria/platform/internal/deadline"
	"github.com/jusassessoria/platform/internal/notifications"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// seedDeadlineWithStatus inserts one intimation + one deadline anchored on recordID, born
// at the given status (bypassing the normal derivation — these tests exercise the
// RESOLUTION path, not creation). Returns the deadline id.
func seedDeadlineWithStatus(t *testing.T, pool *pgxpool.Pool, tenantID, caseID, recordID, status string) string {
	t.Helper()
	ctx := context.Background()
	intimationID := seedIntimationReturningID(t, pool, tenantID, caseID, recordID)
	var deadlineID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO deadline
		   (tenant_id, court_record_id, notification_id, start_date, end_date, days, counting, status)
		 VALUES ($1, $2, $3, DATE '2026-01-07', DATE '2026-01-14', 5, 'BUSINESS', $4)
		 RETURNING id::text`,
		tenantID, recordID, intimationID, status).Scan(&deadlineID); err != nil {
		t.Fatalf("seed deadline (status=%s): %v", status, err)
	}
	return deadlineID
}

// TestOnCourtRecordArchived_ResolvesPendingOpenMissed_LeavesMetIntact is the fatia 2b
// core: 2 PENDING + 1 OPEN + 1 MISSED + 1 MET on the SAME court_record → the four
// non-terminal prazos flip to RESOLVED_ON_CONCLUSION (one deadline_event +
// one deadline.resolved_on_conclusion each), the MET prazo is untouched.
func TestOnCourtRecordArchived_ResolvesPendingOpenMissed_LeavesMetIntact(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-conclusion-batch", 0)

	var caseID, recordID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO court_case (tenant_id) VALUES ($1) RETURNING id::text`, tenantID).Scan(&caseID); err != nil {
		t.Fatalf("seed court_case: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO court_record (tenant_id, case_id, cnj_number, degree, court, completeness, lifecycle)
		 VALUES ($1, $2, $3, 'G1', 'TJRS', 0.9, 'ARCHIVED') RETURNING id::text`,
		tenantID, caseID, "0000012-00.2024.8.26.0012").Scan(&recordID); err != nil {
		t.Fatalf("seed court_record: %v", err)
	}

	resolvableIDs := []string{
		seedDeadlineWithStatus(t, pool, tenantID, caseID, recordID, "PENDING"),
		seedDeadlineWithStatus(t, pool, tenantID, caseID, recordID, "PENDING"),
		seedDeadlineWithStatus(t, pool, tenantID, caseID, recordID, "OPEN"),
		seedDeadlineWithStatus(t, pool, tenantID, caseID, recordID, "MISSED"),
	}
	metID := seedDeadlineWithStatus(t, pool, tenantID, caseID, recordID, "MET")

	uc := deadline.NewUseCase(
		deadline.NewRepository(),
		nil, // no calendar dependency on this path
		events.NewOutbox(),
		deadline.NewDedup(),
		database.NewUnitOfWork(pool),
	)
	ev := deadline.CourtRecordArchived{
		Base:          events.Base{EventID: uuid.NewString(), Aggregate: recordID},
		TenantID:      tenantID,
		CourtRecordID: recordID,
	}
	if err := uc.OnCourtRecordArchived(ctx, ev); err != nil {
		t.Fatalf("OnCourtRecordArchived: %v", err)
	}

	for _, id := range resolvableIDs {
		var status string
		if err := pool.QueryRow(ctx, `SELECT status FROM deadline WHERE id=$1`, id).Scan(&status); err != nil {
			t.Fatalf("read status(%s): %v", id, err)
		}
		if status != "RESOLVED_ON_CONCLUSION" {
			t.Errorf("deadline %s status = %q, want RESOLVED_ON_CONCLUSION", id, status)
		}

		var auditCount int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM deadline_event WHERE deadline_id=$1 AND tipo='resolvido_por_conclusao'`,
			id).Scan(&auditCount); err != nil {
			t.Fatalf("count deadline_event(%s): %v", id, err)
		}
		if auditCount != 1 {
			t.Errorf("deadline_event rows for %s = %d, want 1", id, auditCount)
		}

		var eventCount int
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM outbox
			WHERE type = $1 AND aggregate_type = 'deadline' AND aggregate_id = $2`,
			deadline.TypeDeadlineResolvedOnConclusion, id).Scan(&eventCount); err != nil {
			t.Fatalf("count deadline.resolved_on_conclusion(%s): %v", id, err)
		}
		if eventCount != 1 {
			t.Errorf("deadline.resolved_on_conclusion rows for %s = %d, want 1", id, eventCount)
		}
	}

	var metStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM deadline WHERE id=$1`, metID).Scan(&metStatus); err != nil {
		t.Fatalf("read MET status: %v", err)
	}
	if metStatus != "MET" {
		t.Errorf("MET deadline status = %q, want unchanged MET", metStatus)
	}
	var metEvents int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM outbox WHERE type = $1 AND aggregate_id = $2`,
		deadline.TypeDeadlineResolvedOnConclusion, metID).Scan(&metEvents); err != nil {
		t.Fatalf("count MET events: %v", err)
	}
	if metEvents != 0 {
		t.Errorf("deadline.resolved_on_conclusion rows for the MET prazo = %d, want 0", metEvents)
	}
}

// TestEnrichmentMerge_RepointsDeadlinesAndPublishesSuperseded is the fatia 2a merge path:
// a DJEN placeholder (degree UNKNOWN) grades into a PRE-EXISTING graded record at the same
// (tenant, cnj, degree) — the rare conflict RepointIntimations already handled.
// RepointDeadlines must move the placeholder's deadline the SAME way, and the retiring
// placeholder must announce acquisition.court_record_superseded exactly once.
func TestEnrichmentMerge_RepointsDeadlinesAndPublishesSuperseded(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-conclusion-merge", 0)

	const cnj = "50007978720168210157"

	// The pre-existing GRADED record (already at G1) — the merge target.
	var existingCaseID, existingRecordID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO court_case (tenant_id) VALUES ($1) RETURNING id::text`, tenantID).Scan(&existingCaseID); err != nil {
		t.Fatalf("seed existing court_case: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO court_record (tenant_id, case_id, cnj_number, degree, court, completeness)
		 VALUES ($1, $2, $3, 'G1', 'TJRS', 0.9) RETURNING id::text`,
		tenantID, existingCaseID, cnj).Scan(&existingRecordID); err != nil {
		t.Fatalf("seed existing graded court_record: %v", err)
	}

	// The DJEN UNKNOWN placeholder, with an intimation AND a PENDING deadline anchored to
	// it — exactly the state a merge must repoint.
	var placeholderCaseID, placeholderRecordID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO court_case (tenant_id) VALUES ($1) RETURNING id::text`, tenantID).Scan(&placeholderCaseID); err != nil {
		t.Fatalf("seed placeholder court_case: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO court_record (tenant_id, case_id, cnj_number, degree, court, completeness)
		 VALUES ($1, $2, $3, 'UNKNOWN', 'TJRS', 0.2) RETURNING id::text`,
		tenantID, placeholderCaseID, cnj).Scan(&placeholderRecordID); err != nil {
		t.Fatalf("seed placeholder court_record: %v", err)
	}
	deadlineID := seedDeadlineWithStatus(t, pool, tenantID, placeholderCaseID, placeholderRecordID, "PENDING")

	// Drive the enrichment use case with a canned DATAJUD hit at G1 — the SAME grade the
	// pre-existing record already holds, so GetCourtRecordByKey finds a conflict and takes
	// the merge branch.
	orch := acquisition.NewOrchestrator()
	orch.Register(acquisition.SourceDATAJUD, fakeDATAJUDConnector{body: datajudHitG1(cnj)})
	repo := acquisition.NewRepository(pool)
	uow := database.NewUnitOfWork(pool)
	enrichUC := acquisition.NewEnrichmentUseCase(repo, events.NewOutbox(), uow, orch, acquisition.NewDATAJUDParser())

	ev := acquisition.CourtRecordObserved{
		Base:          events.Base{EventID: uuid.NewString(), Aggregate: placeholderRecordID},
		TenantID:      tenantID,
		CourtRecordID: placeholderRecordID,
		CaseID:        placeholderCaseID,
		CNJNumber:     cnj,
		Degree:        acquisition.DegreeUnknown,
		Court:         "TJRS",
	}
	if err := enrichUC.OnCourtRecordObserved(ctx, ev); err != nil {
		t.Fatalf("OnCourtRecordObserved: %v", err)
	}

	// The placeholder is now SUPERSEDED (retired), the existing record stays graded.
	var placeholderLifecycle string
	if err := pool.QueryRow(ctx,
		`SELECT lifecycle FROM court_record WHERE id=$1`, placeholderRecordID).Scan(&placeholderLifecycle); err != nil {
		t.Fatalf("read placeholder lifecycle: %v", err)
	}
	if placeholderLifecycle != acquisition.LifecycleSuperseded {
		t.Errorf("placeholder lifecycle = %q, want SUPERSEDED", placeholderLifecycle)
	}

	// The production leak this closes: the deadline follows the merge, exactly like the
	// intimation already does.
	var deadlineRecord, intimationRecord string
	if err := pool.QueryRow(ctx,
		`SELECT court_record_id::text FROM deadline WHERE id=$1`, deadlineID).Scan(&deadlineRecord); err != nil {
		t.Fatalf("read deadline.court_record_id: %v", err)
	}
	if deadlineRecord != existingRecordID {
		t.Errorf("deadline.court_record_id = %q, want the merge target %q (RepointDeadlines)", deadlineRecord, existingRecordID)
	}
	if err := pool.QueryRow(ctx,
		`SELECT court_record_id::text FROM intimation WHERE court_record_id=$1 OR court_record_id=$2 LIMIT 1`,
		existingRecordID, placeholderRecordID).Scan(&intimationRecord); err != nil {
		t.Fatalf("read intimation.court_record_id: %v", err)
	}
	if intimationRecord != existingRecordID {
		t.Errorf("intimation.court_record_id = %q, want the merge target %q (RepointIntimations)", intimationRecord, existingRecordID)
	}

	// Exactly one court_record_superseded, aggregate = the RETIRING placeholder.
	var supersededCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM outbox
		WHERE type = $1 AND aggregate_type = 'court_record' AND aggregate_id = $2`,
		acquisition.TypeCourtRecordSuperseded, placeholderRecordID).Scan(&supersededCount); err != nil {
		t.Fatalf("count court_record_superseded: %v", err)
	}
	if supersededCount != 1 {
		t.Errorf("court_record_superseded rows = %d, want 1", supersededCount)
	}
}

// TestNotifications_InApp_DeadlineResolvedOnConclusion_PersistsAvisoWithInfoSeverity
// (fatia 2c.1): a deadline.resolved_on_conclusion persists one low-priority aviso.
// severidade is left unset by record()'s INSERT, so migration 0097's DB default 'info'
// applies — read directly to prove it end to end against the real schema.
func TestNotifications_InApp_DeadlineResolvedOnConclusion_PersistsAvisoWithInfoSeverity(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-inapp-resolved-conclusion", 0)

	uc := newInAppUC(pool)
	ev := notificationsDeadlineResolvedOnConclusion("evt_inapp_roc_1", tenantID)
	if err := uc.OnDeadlineResolvedOnConclusion(ctx, ev); err != nil {
		t.Fatalf("OnDeadlineResolvedOnConclusion: %v", err)
	}

	row, ok := readNotification(t, pool, tenantID)
	if !ok {
		t.Fatal("notification row was not created")
	}
	if row.typ != "deadline_resolved_on_conclusion" {
		t.Fatalf("type = %q, want deadline_resolved_on_conclusion", row.typ)
	}

	var severidade string
	if err := pool.QueryRow(ctx,
		`SELECT severidade FROM notification WHERE tenant_id=$1`, tenantID).Scan(&severidade); err != nil {
		t.Fatalf("read severidade: %v", err)
	}
	if severidade != "info" {
		t.Errorf("severidade = %q, want info (migration 0097 DB default — low priority: resolve trabalho, não cria)", severidade)
	}

	delivery, ok := readDelivery(t, pool, tenantID)
	if !ok {
		t.Fatal("delivery row was not created")
	}
	if delivery.channel != "IN_APP" || delivery.status != "QUEUED" {
		t.Fatalf("delivery = %+v, want IN_APP/QUEUED", delivery)
	}

	// Replay: the same event id creates nothing more.
	if err := uc.OnDeadlineResolvedOnConclusion(ctx, ev); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if n := countNotifications(t, pool, tenantID); n != 1 {
		t.Fatalf("notification rows after replay = %d, want 1", n)
	}
}

// notificationsDeadlineResolvedOnConclusion builds a deadline.resolved_on_conclusion for a
// tenant, in notifications' LOCAL decode shape (mirrors deadlineMissed/deadlineDueSoon in
// notifications_test.go).
func notificationsDeadlineResolvedOnConclusion(eventID, tenantID string) notifications.DeadlineResolvedOnConclusion {
	return notifications.DeadlineResolvedOnConclusion{
		Base:       events.Base{EventID: eventID, Aggregate: uuid.NewString()},
		TenantID:   tenantID,
		DeadlineID: uuid.NewString(),
	}
}

// seedCourtRecordCNJWithLifecycle inserts one court_record (its own court_case) with the
// given cnj_number AND lifecycle, and returns (recordID, caseID) — the fatia 2c fila-ativa
// test needs an ARCHIVED record, unlike seedCourtRecordCNJ's ACTIVE default.
func seedCourtRecordCNJWithLifecycle(t *testing.T, pool *pgxpool.Pool, tenantID, cnj, lifecycle string) (recordID, caseID string) {
	t.Helper()
	ctx := context.Background()
	if err := pool.QueryRow(ctx,
		`INSERT INTO court_case (tenant_id) VALUES ($1) RETURNING id::text`, tenantID).Scan(&caseID); err != nil {
		t.Fatalf("seed court_case: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO court_record (tenant_id, case_id, cnj_number, degree, court, completeness, lifecycle)
		 VALUES ($1, $2, $3, 'G1', 'TJSP', 0.5, $4) RETURNING id::text`,
		tenantID, caseID, cnj, lifecycle).Scan(&recordID); err != nil {
		t.Fatalf("seed court_record: %v", err)
	}
	return recordID, caseID
}

// TestFilaAtiva_ExcludesPendingIntimacaoOnArchivedProcess (fatia 2c.2): a PENDING
// intimação on an ACTIVE process stays in the fila ativa (List + the "pendentes" KPI); the
// SAME triagem state on an ARCHIVED process is excluded from BOTH — it stays reachable only
// through the process detail/history (GetIntimacao/ListIntimacoesByProcesso, not touched by
// this filter). A RESOLVED intimação on the archived process still counts toward
// `resolvidas` — the exclusion targets PENDING specifically, not every row of an archived
// process.
func TestFilaAtiva_ExcludesPendingIntimacaoOnArchivedProcess(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)
	repo := acquisition.NewRepository(pool)
	uc := acquisition.NewReadUseCase(repo)

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-fila-ativa", 0)

	activeRecord, activeCase := seedCourtRecordCNJ(t, pool, tenantID, "0000013-00.2024.8.26.0013")
	seedIntimationWithUserStatus(t, pool, tenantID, activeCase, activeRecord, acquisition.IntimationUserStatusPending)

	archivedRecord, archivedCase := seedCourtRecordCNJWithLifecycle(t, pool, tenantID, "0000014-00.2024.8.26.0014", acquisition.LifecycleArchived)
	seedIntimationWithUserStatus(t, pool, tenantID, archivedCase, archivedRecord, acquisition.IntimationUserStatusPending)
	seedIntimationWithUserStatus(t, pool, tenantID, archivedCase, archivedRecord, acquisition.IntimationUserStatusResolved)

	// SummarizeIntimacoes: pendentes counts ONLY the active-process PENDING row; the
	// archived-process RESOLVED row still counts toward resolvidas (unaffected).
	summary, err := uc.IntimacoesSummary(ctx, tenantID)
	if err != nil {
		t.Fatalf("IntimacoesSummary: %v", err)
	}
	want := acquisition.IntimacoesSummaryView{
		Total: 3, Pendentes: 1, EmAnalise: 0, Resolvidas: 1, Ignoradas: 0, Criticas: 0,
	}
	if summary != want {
		t.Fatalf("summary = %+v, want %+v (archived-process PENDING excluded from pendentes)", summary, want)
	}

	// ListIntimacoes (the "Fila" inbox, no filter — the default active view): the
	// archived-process PENDING row must be ABSENT; the active-process PENDING row and the
	// archived-process RESOLVED row (unaffected — the exclusion targets PENDING
	// specifically, not the whole archived process) must both be present.
	res, err := uc.Intimacoes(ctx, acquisition.IntimacoesQuery{
		TenantID:          tenantID,
		LastMadeAvailable: maxDateLit,
		LastID:            maxUUIDlit,
		Limit:             50,
	})
	if err != nil {
		t.Fatalf("Intimacoes: %v", err)
	}
	var sawActivePending, sawArchivedPending, sawArchivedResolved bool
	for _, item := range res.Items {
		switch {
		case item.CourtRecordID == activeRecord && item.UserStatus == acquisition.IntimationUserStatusPending:
			sawActivePending = true
		case item.CourtRecordID == archivedRecord && item.UserStatus == acquisition.IntimationUserStatusPending:
			sawArchivedPending = true
		case item.CourtRecordID == archivedRecord && item.UserStatus == acquisition.IntimationUserStatusResolved:
			sawArchivedResolved = true
		}
	}
	if !sawActivePending {
		t.Error("active-process PENDING intimação missing from the fila ativa list")
	}
	if sawArchivedPending {
		t.Error("archived-process PENDING intimação still appears in the fila ativa list, want excluded")
	}
	if !sawArchivedResolved {
		t.Error("archived-process RESOLVED intimação missing — the exclusion must target PENDING only, not the whole archived process")
	}
}

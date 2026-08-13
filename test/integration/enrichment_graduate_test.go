//go:build integration

// FIX B — DATAJUD enrichment grades the DJEN UNKNOWN placeholder IN PLACE, against a
// REAL Postgres. The bug it fixes: enrichment used to create a NEW graded record and
// retire the placeholder, orphaning the deadline the deadline slice had already anchored
// to the placeholder (deadline.court_record_id != intimation.court_record_id → ~50% of
// prazos invisible on the Prazos tab). Here we seed a placeholder with an intimation AND
// a deadline both anchored to it, drive the enrichment use case with a canned DATAJUD hit
// (grau G1), and assert the record is mutated on the SAME id — no new record, no
// SUPERSEDED, and the deadline still points at the intimation's court_record.
package integration_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/jusassessoria/platform/internal/acquisition"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// fakeDATAJUDConnector returns a canned DATAJUD ES payload for a by-number fetch, so
// the enrichment runs deterministically without hitting the live CNJ API.
type fakeDATAJUDConnector struct{ body []byte }

func (fakeDATAJUDConnector) ID() string      { return "datajud-fake" }
func (fakeDATAJUDConnector) Version() string { return "test" }
func (fakeDATAJUDConnector) Capabilities() []acquisition.Capability {
	return []acquisition.Capability{acquisition.CapabilityFetchByNumber}
}
func (c fakeDATAJUDConnector) Fetch(_ context.Context, _ acquisition.FetchRequest) (acquisition.RawPayload, error) {
	return acquisition.RawPayload{Source: acquisition.SourceDATAJUD, Body: c.body}, nil
}

// datajudHitG1 is a minimal ES envelope: one process at grau G1 with one movimento.
func datajudHitG1(cnj string) []byte {
	return []byte(`{"hits":{"hits":[{"_source":{` +
		`"numeroProcesso":"` + cnj + `","tribunal":"TJRS","grau":"G1","nivelSigilo":0,` +
		`"classe":{"codigo":1,"nome":"Procedimento Comum"},` +
		`"orgaoJulgador":{"nome":"1ª Vara Cível"},` +
		`"assuntos":[{"codigo":10,"nome":"Indenização"}],` +
		`"movimentos":[{"codigo":123,"nome":"Juntada de Petição","dataHora":"2026-01-02T10:00:00Z"}]}}]}}`)
}

func TestEnrichmentGraduatesPlaceholderInPlace(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-enrich-inplace", 0)

	const cnj = "50007978720168210156"

	// Seed the DJEN placeholder: an UNKNOWN court_record in its own case.
	var caseID, recordID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO court_case (tenant_id) VALUES ($1) RETURNING id::text`, tenantID).Scan(&caseID); err != nil {
		t.Fatalf("seed court_case: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO court_record (tenant_id, case_id, cnj_number, degree, court, completeness)
		 VALUES ($1, $2, $3, 'UNKNOWN', 'TJRS', 0.2) RETURNING id::text`,
		tenantID, caseID, cnj).Scan(&recordID); err != nil {
		t.Fatalf("seed placeholder court_record: %v", err)
	}

	// An intimation and a deadline BOTH anchored to the placeholder — exactly the state
	// the deadline slice leaves behind (deadline.court_record_id = the placeholder id).
	intimationID := seedIntimationReturningID(t, pool, tenantID, caseID, recordID)
	mustExec(t, pool,
		`INSERT INTO deadline
		   (tenant_id, court_record_id, notification_id, start_date, end_date, days, counting, status)
		 VALUES ($1, $2, $3, DATE '2026-01-07', DATE '2026-01-14', 5, 'BUSINESS', 'PENDING')`,
		tenantID, recordID, intimationID)

	// Drive the enrichment use case with the canned DATAJUD hit — the real composition,
	// minus the live connector.
	orch := acquisition.NewOrchestrator()
	orch.Register(acquisition.SourceDATAJUD, fakeDATAJUDConnector{body: datajudHitG1(cnj)})
	repo := acquisition.NewRepository(pool)
	uow := database.NewUnitOfWork(pool)
	enrichUC := acquisition.NewEnrichmentUseCase(repo, events.NewOutbox(), uow, orch, acquisition.NewDATAJUDParser())

	ev := acquisition.CourtRecordObserved{
		Base:          events.Base{EventID: uuid.NewString(), Aggregate: recordID},
		TenantID:      tenantID,
		CourtRecordID: recordID,
		CaseID:        caseID,
		CNJNumber:     cnj,
		Degree:        acquisition.DegreeUnknown,
		Court:         "TJRS",
	}
	if err := enrichUC.OnCourtRecordObserved(ctx, ev); err != nil {
		t.Fatalf("OnCourtRecordObserved: %v", err)
	}

	// The placeholder was graded ON THE SAME id — degree flipped, no new row, no supersede.
	var degree, lifecycle string
	if err := pool.QueryRow(ctx,
		`SELECT degree, lifecycle FROM court_record WHERE id=$1`, recordID).Scan(&degree, &lifecycle); err != nil {
		t.Fatalf("read graded record: %v", err)
	}
	if degree != acquisition.DegreeG1 {
		t.Errorf("placeholder degree = %q, want G1 (graded in place)", degree)
	}
	if lifecycle != acquisition.LifecycleActive {
		t.Errorf("graded record lifecycle = %q, want ACTIVE", lifecycle)
	}

	if total := countRows(t, pool, `SELECT count(*) FROM court_record WHERE tenant_id=$1`, tenantID); total != 1 {
		t.Errorf("court_record count = %d, want 1 (no duplicate created)", total)
	}
	if superseded := countRows(t, pool,
		`SELECT count(*) FROM court_record WHERE tenant_id=$1 AND lifecycle='SUPERSEDED'`, tenantID); superseded != 0 {
		t.Errorf("SUPERSEDED records = %d, want 0 (nothing was retired)", superseded)
	}

	// The invariant that was broken: the intimation stays anchored, and the deadline's
	// court_record matches the intimation's — no orphan.
	var intimationRecord, deadlineRecord string
	if err := pool.QueryRow(ctx,
		`SELECT court_record_id::text FROM intimation WHERE id=$1`, intimationID).Scan(&intimationRecord); err != nil {
		t.Fatalf("read intimation: %v", err)
	}
	if intimationRecord != recordID {
		t.Errorf("intimation.court_record_id = %q, want the stable placeholder id %q", intimationRecord, recordID)
	}
	if err := pool.QueryRow(ctx,
		`SELECT court_record_id::text FROM deadline WHERE notification_id=$1`, intimationID).Scan(&deadlineRecord); err != nil {
		t.Fatalf("read deadline: %v", err)
	}
	if deadlineRecord != intimationRecord {
		t.Errorf("deadline.court_record_id = %q, want intimation.court_record_id %q (no orphan)", deadlineRecord, intimationRecord)
	}

	// The DATAJUD movimento landed as a docket entry on the graded record.
	if docket := countRows(t, pool,
		`SELECT count(*) FROM docket_entry WHERE court_record_id=$1`, recordID); docket != 1 {
		t.Errorf("docket entries on graded record = %d, want 1", docket)
	}
}

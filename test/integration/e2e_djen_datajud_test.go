//go:build integration

// End-to-end of the whole acquisition flow against a REAL Postgres and the LIVE
// CNJ APIs, driven by real OABs: DJEN discovers communications by OAB over a
// window → court records (degree UNKNOWN) + intimations land → each observed
// record is enriched via DATAJUD (by number) → the grade is revealed and the
// placeholder+merge runs (graded record, intimations re-pointed, placeholder
// SUPERSEDED, movimentos as docket entries).
//
// It hits the internet (comunicaapi.pje.jus.br + api-publica.datajud.cnj.jus.br),
// so it is guarded by the `integration` tag AND is resilient: a WAF 403 or an
// empty window t.Skip()s rather than failing, and it caps how many records it
// enriches so it stays within the DATAJUD rate limit. Run with:
//
//	go test -tags=integration -run TestE2E_DJEN_DATAJUD -v ./test/integration/
package integration_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jusassessoria/platform/internal/acquisition"
	"github.com/jusassessoria/platform/lib/calendar"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// e2eOABs are the registrations under test (UF+number, the scope format). Two SP
// and one MG lawyer — real, public.
var e2eOABs = []string{"SP347019", "SP321511", "MG199303"}

const (
	// A short recent window keeps discovery volume (and so the DB writes) modest.
	e2eWindowFrom = "2026-07-30"
	e2eWindowTo   = "2026-07-31"
	// Cap how many discovered records we enrich, to stay inside the DATAJUD rate
	// limit and keep the run quick — the flow is identical for all of them.
	e2eEnrichCap = 6
)

func TestE2E_DJEN_DATAJUD(t *testing.T) {
	// This test hits the LIVE CNJ APIs (DJEN + DATAJUD), so it is opt-in: CI runs
	// `-tags=integration` but must NOT depend on external gov endpoints (WAF/IP
	// blocks would flake the pipeline). Run it locally with E2E_LIVE=1.
	if os.Getenv("E2E_LIVE") == "" {
		t.Skip("live-API e2e — set E2E_LIVE=1 to run (kept out of CI)")
	}

	pool := newPool(t)
	ctx := context.Background()
	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-e2e", 0)
	integID := seedIntegration(t, pool, tenantID, acquisition.SourceDJEN)

	// Real connectors, real parsers, real outbox — the exact worker composition.
	orch := acquisition.NewOrchestrator()
	orch.Register(acquisition.SourceDJEN, acquisition.NewDJENConnector())
	orch.Register(acquisition.SourceDATAJUD, acquisition.NewDATAJUDConnector())
	cal := calendar.New(calendar.NewStore(pool))
	parser := acquisition.ParserSet{acquisition.NewDJENParser(cal), acquisition.NewDATAJUDParser()}
	repo := acquisition.NewRepository(pool)
	outbox := events.NewOutbox()
	uow := database.NewUnitOfWork(pool)

	syncUC := acquisition.NewSyncUseCase(repo, outbox, uow, orch, parser)
	enrichUC := acquisition.NewEnrichmentUseCase(repo, outbox, uow, orch, parser)

	// ── Phase 1: DJEN discovery ────────────────────────────────────────────────
	syncEv := acquisition.SyncRequested{
		Base:          events.Base{EventID: uuid.NewString(), Aggregate: integID},
		BackfillJobID: uuid.NewString(),
		TenantID:      tenantID,
		IntegrationID: integID,
		Source:        acquisition.SourceDJEN,
		WindowFrom:    e2eWindowFrom,
		WindowTo:      e2eWindowTo,
		Scope:         acquisition.Scope{OAB: e2eOABs},
	}

	t.Logf("── Phase 1: DJEN discovery for OABs %v over %s..%s", e2eOABs, e2eWindowFrom, e2eWindowTo)
	if err := syncUC.OnSyncRequested(ctx, syncEv); err != nil {
		t.Fatalf("OnSyncRequested: %v", err)
	}

	var runStatus string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM sync_run WHERE integration_id=$1 ORDER BY started_at DESC LIMIT 1`, integID).
		Scan(&runStatus); err != nil {
		t.Fatalf("read sync_run: %v", err)
	}
	t.Logf("   sync_run status = %s", runStatus)
	if runStatus == acquisition.SyncStatusFailed {
		t.Skip("DJEN fetch failed (WAF/transport) — cannot exercise the flow this run")
	}

	discovered := countRows(t, pool, `SELECT count(*) FROM court_record WHERE tenant_id=$1`, tenantID)
	intimations := countRows(t, pool, `SELECT count(*) FROM intimation WHERE tenant_id=$1`, tenantID)
	t.Logf("   discovered: %d court records, %d intimations", discovered, intimations)
	if discovered == 0 {
		t.Skip("DJEN returned no communications for this window — nothing to enrich")
	}

	// All discovery-time records are UNKNOWN placeholders (DJEN never grades).
	unknown := countRows(t, pool,
		`SELECT count(*) FROM court_record WHERE tenant_id=$1 AND degree='UNKNOWN'`, tenantID)
	if unknown != discovered {
		t.Errorf("expected every discovered record UNKNOWN, got %d/%d", unknown, discovered)
	}
	// A discovered intimation carries the DJEN fields and a derived deadline_start.
	sampleIntimationInvariants(t, pool, tenantID)

	// ── Phase 2: DATAJUD enrichment (placeholder+merge) ────────────────────────
	// Drive it from the court_record_observed events the sync wrote to the outbox —
	// the same trigger the worker's enrichment listener consumes.
	observed := readObservedFromOutbox(t, pool, e2eEnrichCap)
	t.Logf("── Phase 2: enriching %d of the discovered records via DATAJUD", len(observed))

	enrichedOK, hits := 0, 0
	for _, obs := range observed {
		before := countRows(t, pool,
			`SELECT count(*) FROM court_record WHERE tenant_id=$1 AND cnj_number=$2 AND degree<>'UNKNOWN'`,
			tenantID, obs.CNJNumber)
		if err := enrichUC.OnCourtRecordObserved(ctx, obs); err != nil {
			t.Fatalf("OnCourtRecordObserved(%s): %v", obs.CNJNumber, err)
		}
		enrichedOK++
		after := countRows(t, pool,
			`SELECT count(*) FROM court_record WHERE tenant_id=$1 AND cnj_number=$2 AND degree<>'UNKNOWN'`,
			tenantID, obs.CNJNumber)
		if after > before {
			hits++
			t.Logf("   %s @ %s → graded (DATAJUD hit)", obs.CNJNumber, obs.Court)
		} else {
			t.Logf("   %s @ %s → no DATAJUD hit (not indexed yet)", obs.CNJNumber, obs.Court)
		}
	}
	if enrichedOK != len(observed) {
		t.Fatalf("enriched %d of %d without error", enrichedOK, len(observed))
	}

	// ── Final state ────────────────────────────────────────────────────────────
	graded := countRows(t, pool,
		`SELECT count(*) FROM court_record WHERE tenant_id=$1 AND degree<>'UNKNOWN' AND lifecycle='ACTIVE'`, tenantID)
	superseded := countRows(t, pool,
		`SELECT count(*) FROM court_record WHERE tenant_id=$1 AND lifecycle='SUPERSEDED'`, tenantID)
	docket := countRows(t, pool,
		`SELECT count(*) FROM docket_entry de JOIN court_record cr ON cr.id=de.court_record_id WHERE cr.tenant_id=$1`, tenantID)
	t.Logf("── Final: %d graded (ACTIVE), %d superseded placeholders, %d docket entries; %d/%d DATAJUD hits",
		graded, superseded, docket, hits, len(observed))

	// If DATAJUD had at least one hit, the placeholder+merge must have produced a
	// graded ACTIVE record, retired the placeholder, and attached movimentos — and
	// every graded record's intimations must have followed it (none left on a
	// superseded placeholder).
	if hits > 0 {
		if graded == 0 || superseded == 0 || docket == 0 {
			t.Errorf("a DATAJUD hit did not complete the merge: graded=%d superseded=%d docket=%d", graded, superseded, docket)
		}
		orphaned := countRows(t, pool,
			`SELECT count(*) FROM intimation i
			 JOIN court_record cr ON cr.id=i.court_record_id
			 WHERE i.tenant_id=$1 AND cr.lifecycle='SUPERSEDED'`, tenantID)
		if orphaned != 0 {
			t.Errorf("%d intimations left on a SUPERSEDED placeholder — re-point failed", orphaned)
		}
		// A graded record carries a next_sync_at (it entered the re-poll schedule).
		scheduled := countRows(t, pool,
			`SELECT count(*) FROM court_record WHERE tenant_id=$1 AND degree<>'UNKNOWN' AND next_sync_at IS NOT NULL`, tenantID)
		if scheduled == 0 {
			t.Error("no graded record has next_sync_at — it will never be re-polled")
		}
	} else {
		t.Log("no DATAJUD hits this run (processes not yet indexed) — merge assertions skipped")
	}

	// ── Phase 3: the screen reads (what the FE renders) ────────────────────────
	readUC := acquisition.NewReadUseCase(repo)

	procsRes, err := readUC.Processos(ctx, acquisition.ProcessosQuery{
		TenantID: tenantID, LastCNJ: "", LastID: "00000000-0000-0000-0000-000000000000", Limit: 5,
	})
	if err != nil {
		t.Fatalf("read Processos: %v", err)
	}
	procs := procsRes.Items
	if len(procs) == 0 {
		t.Error("Processos read returned nothing, but records were discovered")
	}
	t.Logf("── Phase 3: GET /v1/processos → %d rows (first %d shown)", discovered, len(procs))
	for _, p := range procs {
		last := "—"
		if p.LastMovementAt != nil {
			last = p.LastMovementAt.Format("2006-01-02") + " " + truncate(p.LastMovementText, 40)
		}
		t.Logf("   • %s [%s] %s — classe=%q · último andamento: %s", p.CNJNumber, p.Degree, p.Court, p.Class, last)
	}

	intimsRes, err := readUC.Intimacoes(ctx, acquisition.IntimacoesQuery{
		TenantID: tenantID, LastMadeAvailable: "9999-12-31", LastID: "ffffffff-ffff-ffff-ffff-ffffffffffff", Limit: 5,
	})
	if err != nil {
		t.Fatalf("read Intimacoes: %v", err)
	}
	intims := intimsRes.Items
	if len(intims) == 0 {
		t.Error("Intimacoes read returned nothing, but intimations were discovered")
	}
	t.Logf("── GET /v1/intimacoes → %d rows (first %d shown)", intimations, len(intims))
	for _, in := range intims {
		t.Logf("   • %s [%s] %s tipo=%s status=%s · disp=%s prazo_início=%s",
			in.CNJNumber, in.Degree, in.Court, in.Type, in.Status,
			in.MadeAvailableAt.Format("2006-01-02"), in.DeadlineStartAt.Format("2006-01-02"))
	}
}

// truncate shortens s to n runes for the log trace.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// sampleIntimationInvariants checks one discovered intimation carries a derived
// deadline_start_at (>= made_available_at) and a normalized type.
func sampleIntimationInvariants(t *testing.T, pool *pgxpool.Pool, tenantID string) {
	t.Helper()
	var typ string
	var madeAvailable, deadlineStart string
	err := pool.QueryRow(context.Background(),
		`SELECT type, made_available_at::text, deadline_start_at::text
		 FROM intimation WHERE tenant_id=$1 ORDER BY made_available_at LIMIT 1`, tenantID).
		Scan(&typ, &madeAvailable, &deadlineStart)
	if err != nil {
		t.Fatalf("read sample intimation: %v", err)
	}
	if typ == "" {
		t.Error("intimation type was not normalized")
	}
	if deadlineStart < madeAvailable {
		t.Errorf("deadline_start_at %s precedes made_available_at %s", deadlineStart, madeAvailable)
	}
	t.Logf("   sample intimation: type=%s made_available=%s deadline_start=%s", typ, madeAvailable, deadlineStart)
}

// readObservedFromOutbox decodes up to limit court_record_observed events the sync
// cycle wrote — the real trigger the enrichment consumer reacts to.
func readObservedFromOutbox(t *testing.T, pool *pgxpool.Pool, limit int) []acquisition.CourtRecordObserved {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT payload FROM outbox WHERE type=$1 ORDER BY id LIMIT $2`,
		acquisition.TypeCourtRecordObserved, limit)
	if err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	defer rows.Close()

	var out []acquisition.CourtRecordObserved
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			t.Fatalf("scan outbox payload: %v", err)
		}
		var ev acquisition.CourtRecordObserved
		if err := json.Unmarshal(payload, &ev); err != nil {
			t.Fatalf("decode court_record_observed: %v", err)
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate outbox: %v", err)
	}
	return out
}

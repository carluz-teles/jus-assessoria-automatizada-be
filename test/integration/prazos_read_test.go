//go:build integration

// Prazos read-model integration tests — prove the fatia 3 read surface (GET
// /v1/processos/:id/prazos, GET /v1/prazos, GET /v1/prazos/:id) against a REAL Postgres
// with every migration applied. A deadline is SEEDED through the real 2c creation use
// case (listener path) so the reads run over a genuinely-derived prazo (born OPEN under
// the default seletiva policy — no prazo_declarado, no divergence — see
// TestDeadline_Observed_DerivesOpenDeadlineAndEvent), and the tests assert the read models
// carry the right fields (days_left computed in SQL, confirmed=false, holidays_applied,
// the process context) and that the agenda's ascending (end_date, id) keyset pages
// correctly. Each test uses a fresh tenant, so the tenant filter (barrier 1) isolates
// counts on the shared DB.
package integration_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jusassessoria/platform/internal/deadline"
)

// newDeadlineReader wires the pool-backed read use case — exactly the api's composition
// (deadline.NewHandler over this reader).
func newDeadlineReader(pool *pgxpool.Pool) *deadline.ReadUseCase {
	return deadline.NewReadUseCase(deadline.NewReadRepository(pool))
}

// deadlineIDFor reads the id the derivation assigned to the prazo of an intimação.
func deadlineIDFor(ctx context.Context, t *testing.T, pool *pgxpool.Pool, intimationID uuid.UUID) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx,
		`SELECT id::text FROM deadline WHERE notification_id = $1`, intimationID).Scan(&id); err != nil {
		t.Fatalf("read deadline id: %v", err)
	}
	return id
}

// expectedDaysLeft reads what (end_date - CURRENT_DATE)::int evaluates to for the prazo,
// so the assertion tracks the DB clock instead of hard-coding a value that drifts daily.
func expectedDaysLeft(ctx context.Context, t *testing.T, pool *pgxpool.Pool, deadlineID string) int {
	t.Helper()
	var days int
	if err := pool.QueryRow(ctx,
		`SELECT (end_date - CURRENT_DATE)::int FROM deadline WHERE id = $1`, deadlineID).Scan(&days); err != nil {
		t.Fatalf("read expected days_left: %v", err)
	}
	return days
}

// PR1: a derived prazo is returned by all three read endpoints with the right fields — the
// Prazos tab, the agenda (with process context), and the detail (with rules_version). A
// foreign tenant gets a typed 404 on the detail.
func TestPrazosRead_ThreeEndpoints_ReturnDerivedDeadline(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)
	p := seedDeadlineParentsCommitted(ctx, t, pool)

	// INTIMACAO on a cível court → the seeded MANIFESTACAO/5/BUSINESS rule, end 2024-03-11.
	ev := observedFor(p, uuid.NewString(), "INTIMACAO", "TJSP", "SP", "2024-03-04")
	if err := newDeadlineUCAt(pool, deadlineTestNow).OnIntimationObserved(ctx, ev); err != nil {
		t.Fatalf("OnIntimationObserved() error = %v", err)
	}
	deadlineID := deadlineIDFor(ctx, t, pool, p.intimationID)
	wantDaysLeft := expectedDaysLeft(ctx, t, pool, deadlineID)

	tenant := p.tenantID.String()
	reader := newDeadlineReader(pool)

	// (1) GET /v1/processos/:id/prazos
	byProc, err := reader.PrazosByProcesso(ctx, deadline.PrazosByProcessoQuery{
		TenantID: tenant, CourtRecordID: p.courtRecordID.String(),
		LastEnd: "0001-01-01", LastID: "00000000-0000-0000-0000-000000000000", Limit: 20,
	})
	if err != nil {
		t.Fatalf("PrazosByProcesso: %v", err)
	}
	if len(byProc.Items) != 1 || byProc.Total != 1 {
		t.Fatalf("tab items/total = %d/%d, want 1/1", len(byProc.Items), byProc.Total)
	}
	row := byProc.Items[0]
	if row.ID != deadlineID {
		t.Errorf("row.ID = %q, want %q", row.ID, deadlineID)
	}
	// OPEN, not PENDING: this tenant has no seeded deadline_policy (default seletiva), and the
	// derivation is calculado/not-divergent, so confirmacao_exigida is false — the prazo is
	// born OPEN directly (see TestDeadline_Observed_DerivesOpenDeadlineAndEvent).
	if row.Kind != "MANIFESTACAO" || row.Counting != "BUSINESS" || row.Status != "OPEN" {
		t.Errorf("kind/counting/status = %q/%q/%q, want MANIFESTACAO/BUSINESS/OPEN", row.Kind, row.Counting, row.Status)
	}
	if row.Confirmed {
		t.Error("Confirmed = true, want false (no human aval yet)")
	}
	if row.IntimationID != p.intimationID.String() {
		t.Errorf("IntimationID = %q, want %q", row.IntimationID, p.intimationID)
	}
	if row.HolidaysApplied == nil {
		t.Error("HolidaysApplied = nil, want a (possibly empty) slice")
	}
	if row.DaysLeft != wantDaysLeft {
		t.Errorf("DaysLeft = %d, want %d ((end_date - CURRENT_DATE))", row.DaysLeft, wantDaysLeft)
	}

	// (2) GET /v1/prazos — the agenda carries the process context from the join.
	agenda, err := reader.Prazos(ctx, deadline.PrazosQuery{
		TenantID: tenant, LastEnd: "0001-01-01", LastID: "00000000-0000-0000-0000-000000000000", Limit: 20,
	})
	if err != nil {
		t.Fatalf("Prazos: %v", err)
	}
	if len(agenda.Items) != 1 || agenda.TotalCount != 1 || agenda.Total != 1 {
		t.Fatalf("agenda items/x/y = %d/%d/%d, want 1/1/1", len(agenda.Items), agenda.TotalCount, agenda.Total)
	}
	ag := agenda.Items[0]
	if ag.Court != "TJSP" || ag.CNJNumber == "" || ag.CourtRecordID != p.courtRecordID.String() {
		t.Errorf("agenda context = court %q / cnj %q / cr %q, want TJSP / non-empty / %q",
			ag.Court, ag.CNJNumber, ag.CourtRecordID, p.courtRecordID)
	}

	// (3) GET /v1/prazos/:id — the detail carries rules_version + days.
	detail, err := reader.Prazo(ctx, tenant, deadlineID)
	if err != nil {
		t.Fatalf("Prazo: %v", err)
	}
	if detail.RulesVersion != "v0" || detail.Days != 5 || detail.Source != "RULE" {
		t.Errorf("detail rules/days/source = %q/%d/%q, want v0/5/RULE", detail.RulesVersion, detail.Days, detail.Source)
	}
	if detail.IntimationID != p.intimationID.String() {
		t.Errorf("detail IntimationID = %q, want %q", detail.IntimationID, p.intimationID)
	}

	// A foreign tenant never sees the prazo — typed not-found, never (nil, nil).
	if _, err := reader.Prazo(ctx, uuid.NewString(), deadlineID); !errors.Is(err, deadline.ErrDeadlineNotFound) {
		t.Errorf("cross-tenant Prazo err = %v, want ErrDeadlineNotFound", err)
	}
}

// PR2: the agenda's ascending (end_date, id) keyset pages the tenant's prazos soonest
// first — two derived prazos with different end dates come back in order across two pages
// of one, and the "X de Y" totals see both.
func TestPrazosRead_Agenda_KeysetPaginatesAscending(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)
	p := seedDeadlineParentsCommitted(ctx, t, pool)
	tenant := p.tenantID.String()

	// Prazo A: INTIMACAO start 2024-03-04 → end 2024-03-11 (earlier).
	evA := observedFor(p, uuid.NewString(), "INTIMACAO", "TJSP", "SP", "2024-03-04")
	if err := newDeadlineUC(pool).OnIntimationObserved(ctx, evA); err != nil {
		t.Fatalf("derive A: %v", err)
	}
	idA := deadlineIDFor(ctx, t, pool, p.intimationID)

	// Prazo B: a second intimação on the same tenant/record, start 2024-06-10 → later end.
	intimBID := seedSecondIntimation(ctx, t, pool, p)
	pB := deadlineParents{tenantID: p.tenantID, courtRecordID: p.courtRecordID, intimationID: intimBID}
	evB := observedFor(pB, uuid.NewString(), "INTIMACAO", "TJSP", "SP", "2024-06-10")
	if err := newDeadlineUC(pool).OnIntimationObserved(ctx, evB); err != nil {
		t.Fatalf("derive B: %v", err)
	}
	idB := deadlineIDFor(ctx, t, pool, intimBID)

	reader := newDeadlineReader(pool)

	// Page 1 (limit 1): the earliest prazo (A), and a further page remains.
	page1, err := reader.Prazos(ctx, deadline.PrazosQuery{
		TenantID: tenant, LastEnd: "0001-01-01", LastID: "00000000-0000-0000-0000-000000000000", Limit: 1,
	})
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if len(page1.Items) != 1 || page1.Items[0].ID != idA {
		t.Fatalf("page 1 = %+v, want single prazo A (%s)", ids(page1.Items), idA)
	}
	if !page1.HasMore {
		t.Fatal("page 1 HasMore = false, want true (B remains)")
	}
	if page1.TotalCount != 2 || page1.Total != 2 {
		t.Errorf("page 1 totals = %d/%d, want 2/2", page1.TotalCount, page1.Total)
	}

	// Page 2: resume from A's keyset → the later prazo (B), no further page.
	last := page1.Items[0]
	page2, err := reader.Prazos(ctx, deadline.PrazosQuery{
		TenantID: tenant, LastEnd: last.EndDate.Format("2006-01-02"), LastID: last.ID, Limit: 1,
	})
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if len(page2.Items) != 1 || page2.Items[0].ID != idB {
		t.Fatalf("page 2 = %+v, want single prazo B (%s)", ids(page2.Items), idB)
	}
	if page2.HasMore {
		t.Error("page 2 HasMore = true, want false (B is last)")
	}
	if !page1.Items[0].EndDate.Before(page2.Items[0].EndDate) {
		t.Errorf("ordering: A end %v not before B end %v", page1.Items[0].EndDate, page2.Items[0].EndDate)
	}
}

// PR3: the F2 lookup (GET /v1/prazos?intimation_id=...) returns exactly the prazo derived
// from that intimação — one item carrying the right intimation_id and the process context
// — while a foreign tenant or an intimação with no prazo sees an empty result (never an
// error). Proves the 1:1 notification_id filter against a real derivation.
func TestPrazosRead_ByIntimacao_ReturnsSingleDerivedDeadline(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)
	p := seedDeadlineParentsCommitted(ctx, t, pool)

	ev := observedFor(p, uuid.NewString(), "INTIMACAO", "TJSP", "SP", "2024-03-04")
	if err := newDeadlineUC(pool).OnIntimationObserved(ctx, ev); err != nil {
		t.Fatalf("OnIntimationObserved() error = %v", err)
	}
	deadlineID := deadlineIDFor(ctx, t, pool, p.intimationID)

	tenant := p.tenantID.String()
	reader := newDeadlineReader(pool)

	res, err := reader.PrazosByIntimacao(ctx, tenant, p.intimationID.String())
	if err != nil {
		t.Fatalf("PrazosByIntimacao: %v", err)
	}
	if len(res.Items) != 1 || res.TotalCount != 1 || res.Total != 1 || res.HasMore {
		t.Fatalf("result = items %d / totals %d,%d / hasMore %v, want 1/1,1/false",
			len(res.Items), res.TotalCount, res.Total, res.HasMore)
	}
	row := res.Items[0]
	if row.ID != deadlineID {
		t.Errorf("row.ID = %q, want %q", row.ID, deadlineID)
	}
	if row.IntimationID != p.intimationID.String() {
		t.Errorf("IntimationID = %q, want %q", row.IntimationID, p.intimationID)
	}
	if row.Court != "TJSP" || row.CourtRecordID != p.courtRecordID.String() {
		t.Errorf("context = court %q / cr %q, want TJSP / %q", row.Court, row.CourtRecordID, p.courtRecordID)
	}

	// A foreign tenant sees no prazo for the same intimação — empty, never an error.
	foreign, err := reader.PrazosByIntimacao(ctx, uuid.NewString(), p.intimationID.String())
	if err != nil {
		t.Fatalf("cross-tenant PrazosByIntimacao: %v", err)
	}
	if len(foreign.Items) != 0 || foreign.Total != 0 {
		t.Errorf("cross-tenant result = items %d / total %d, want 0/0", len(foreign.Items), foreign.Total)
	}

	// An intimação with no derived prazo is an empty result, not a 404.
	none, err := reader.PrazosByIntimacao(ctx, tenant, uuid.NewString())
	if err != nil {
		t.Fatalf("no-prazo PrazosByIntimacao: %v", err)
	}
	if len(none.Items) != 0 || none.Total != 0 {
		t.Errorf("no-prazo result = items %d / total %d, want 0/0", len(none.Items), none.Total)
	}
}

// seedSecondIntimation inserts a second intimation on the same tenant/case/record as the
// seeded parents (a distinct notification_id so a second deadline can hang off it), and
// returns its id. Committed so the derivation use case's own tx can reference it.
func seedSecondIntimation(ctx context.Context, t *testing.T, pool *pgxpool.Pool, p deadlineParents) uuid.UUID {
	t.Helper()
	var caseID uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT case_id FROM court_record WHERE id = $1`, p.courtRecordID).Scan(&caseID); err != nil {
		t.Fatalf("read case_id: %v", err)
	}
	var id uuid.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO intimation
			(tenant_id, case_id, court_record_id, hash,
			 made_available_at, published_at, deadline_start_at, content, source)
		VALUES ($1, $2, $3, $4, '2024-06-09', '2024-06-09', '2024-06-10', 'teor B', 'DJEN')
		RETURNING id`,
		p.tenantID, caseID, p.courtRecordID, uuid.NewString()).Scan(&id); err != nil {
		t.Fatalf("seed second intimation: %v", err)
	}
	return id
}

// ids is a tiny debug helper: the ids of a prazo page, for failure messages.
func ids(items []deadline.AgendaPrazoView) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.ID
	}
	return out
}

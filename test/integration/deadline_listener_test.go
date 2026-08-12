//go:build integration

// Deadline listener integration tests — prove the CREATION path (slice 2c) end to end
// against a REAL Postgres with every migration applied: the use case consumes an
// acquisition.intimation.observed, resolves the SEEDED deadline_rule (0024), computes
// the end date through the REAL judicial calendar (holiday table), and commits — in ONE
// tx — a deadline born PENDING plus a deadline.opened outbox row. It also proves the two
// idempotency floors (event dedup + the 1:1 notification_id UNIQUE) and that a real
// holiday between start and vencimento shifts the date and is audited.
//
// These drive the real use case (real repo + calendar + outbox + dedup + uow), not the
// asynq listener — the handler is a thin decode+delegate covered by unit tests. Parent
// rows (tenant → case → record → intimation) are COMMITTED so the use case's own tx can
// reference them; each test uses a fresh tenant, so counts are isolated on the shared DB.
package integration_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jusassessoria/platform/internal/deadline"
	"github.com/jusassessoria/platform/lib/calendar"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// newDeadlineUC wires the real creation use case over the pool: sqlc repo, the
// Postgres-backed calendar, the transactional outbox, the stateless dedup and the unit
// of work — exactly the worker's composition.
func newDeadlineUC(pool *pgxpool.Pool) *deadline.UseCase {
	return deadline.NewUseCase(
		deadline.NewRepository(),
		calendar.New(calendar.NewStore(pool)),
		events.NewOutbox(),
		deadline.NewDedup(),
		database.NewUnitOfWork(pool),
	)
}

// seedDeadlineParentsCommitted seeds the tenant → case → record → intimation chain and
// COMMITS it (the rolled-back seedDeadlineParents is for pure schema tests). Returns the
// ids the derived prazo anchors on.
func seedDeadlineParentsCommitted(ctx context.Context, t *testing.T, pool *pgxpool.Pool) deadlineParents {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	p := seedDeadlineParents(ctx, t, tx)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit parents: %v", err)
	}
	return p
}

// observedFor builds an intimation.observed for the seeded chain. caseID is unused by
// the derivation, so it is left empty; the type/court/uf/start drive the rule and math.
func observedFor(p deadlineParents, eventID, typ, court, uf, start string) deadline.IntimationObserved {
	return deadline.IntimationObserved{
		Base:            events.Base{EventID: eventID, Aggregate: p.intimationID.String()},
		TenantID:        p.tenantID.String(),
		IntimationID:    p.intimationID.String(),
		CourtRecordID:   p.courtRecordID.String(),
		Type:            typ,
		Court:           court,
		UF:              uf,
		DeadlineStartAt: start,
	}
}

// DL1: a fresh observed intimação derives exactly one PENDING deadline (source RULE,
// kind/days/counting from the seeded rule) AND one deadline.opened, in the same tx, with
// the deadline id as the outbox aggregate.
func TestDeadline_Observed_DerivesPendingDeadlineAndEvent(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)
	p := seedDeadlineParentsCommitted(ctx, t, pool)

	// INTIMACAO on a cível court → the seeded MANIFESTACAO/5/BUSINESS rule.
	ev := observedFor(p, uuid.NewString(), "INTIMACAO", "TJSP", "SP", "2024-03-04")
	if err := newDeadlineUC(pool).OnIntimationObserved(ctx, ev); err != nil {
		t.Fatalf("OnIntimationObserved() error = %v", err)
	}

	var (
		id, status, source, kind, counting, rulesVersion string
		days                                             int
		startDate, endDate                               string
		tenantID                                         string
		holidaysRaw                                      []byte
	)
	if err := pool.QueryRow(ctx, `
		SELECT id, status, source, kind, days, counting, rules_version,
		       start_date::text, end_date::text, tenant_id::text, holidays_applied
		FROM deadline WHERE notification_id = $1`, p.intimationID).
		Scan(&id, &status, &source, &kind, &days, &counting, &rulesVersion,
			&startDate, &endDate, &tenantID, &holidaysRaw); err != nil {
		t.Fatalf("read deadline: %v", err)
	}

	if status != "PENDING" {
		t.Errorf("status = %q, want PENDING", status)
	}
	if source != "RULE" || kind != "MANIFESTACAO" || days != 5 || counting != "BUSINESS" {
		t.Errorf("source/kind/days/counting = %q/%q/%d/%q, want RULE/MANIFESTACAO/5/BUSINESS", source, kind, days, counting)
	}
	if rulesVersion != "v0" {
		t.Errorf("rules_version = %q, want v0", rulesVersion)
	}
	if startDate != "2024-03-04" || endDate != "2024-03-11" {
		t.Errorf("start/end = %q/%q, want 2024-03-04/2024-03-11", startDate, endDate)
	}
	if tenantID != p.tenantID.String() {
		t.Errorf("tenant_id = %q, want %q", tenantID, p.tenantID)
	}

	// confirmed_by/at NULL (no human aval yet).
	var confirmedBy, confirmedAt *string
	if err := pool.QueryRow(ctx,
		`SELECT confirmed_by::text, confirmed_at::text FROM deadline WHERE id = $1`, id).
		Scan(&confirmedBy, &confirmedAt); err != nil {
		t.Fatalf("read confirmed_*: %v", err)
	}
	if confirmedBy != nil || confirmedAt != nil {
		t.Errorf("confirmed_by/at = %v/%v, want NULL/NULL", confirmedBy, confirmedAt)
	}

	// Exactly one deadline.opened, aggregate = the deadline id.
	var openCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM outbox
		WHERE type = $1 AND aggregate_type = 'deadline' AND aggregate_id = $2`,
		deadline.TypeDeadlineOpened, id).Scan(&openCount); err != nil {
		t.Fatalf("count deadline.opened: %v", err)
	}
	if openCount != 1 {
		t.Fatalf("deadline.opened rows = %d, want 1", openCount)
	}

	var payloadDeadlineID, payloadKind, payloadEnd, payloadCounting string
	if err := pool.QueryRow(ctx, `
		SELECT payload->>'deadline_id', payload->>'kind', payload->>'end_date', payload->>'counting'
		FROM outbox WHERE type = $1 AND aggregate_id = $2`,
		deadline.TypeDeadlineOpened, id).
		Scan(&payloadDeadlineID, &payloadKind, &payloadEnd, &payloadCounting); err != nil {
		t.Fatalf("read deadline.opened payload: %v", err)
	}
	if payloadDeadlineID != id || payloadKind != "MANIFESTACAO" || payloadEnd != "2024-03-11" || payloadCounting != "BUSINESS" {
		t.Errorf("payload deadline_id/kind/end/counting = %q/%q/%q/%q", payloadDeadlineID, payloadKind, payloadEnd, payloadCounting)
	}
}

// DL2: a double delivery of the SAME event_id derives exactly one deadline (the
// processed_event dedup, marked in the write tx).
func TestDeadline_Observed_DedupOnReplay(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)
	p := seedDeadlineParentsCommitted(ctx, t, pool)
	uc := newDeadlineUC(pool)

	ev := observedFor(p, uuid.NewString(), "INTIMACAO", "TJSP", "SP", "2024-03-04")
	for i := 0; i < 2; i++ {
		if err := uc.OnIntimationObserved(ctx, ev); err != nil {
			t.Fatalf("delivery %d error = %v", i, err)
		}
	}

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM deadline WHERE notification_id = $1`, p.intimationID).Scan(&n); err != nil {
		t.Fatalf("count deadline: %v", err)
	}
	if n != 1 {
		t.Errorf("deadline rows after replay = %d, want 1 (dedup)", n)
	}
}

// DL3: a DIFFERENT event_id for the same intimação (past the dedup) still derives only
// one deadline — the 1:1 notification_id UNIQUE bars the phantom, and no second
// deadline.opened is emitted.
func TestDeadline_Observed_UniqueBarsPhantom(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)
	p := seedDeadlineParentsCommitted(ctx, t, pool)
	uc := newDeadlineUC(pool)

	first := observedFor(p, uuid.NewString(), "INTIMACAO", "TJSP", "SP", "2024-03-04")
	second := observedFor(p, uuid.NewString(), "INTIMACAO", "TJSP", "SP", "2024-03-04") // new event_id, same intimação
	if err := uc.OnIntimationObserved(ctx, first); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	if err := uc.OnIntimationObserved(ctx, second); err != nil {
		t.Fatalf("second delivery (should no-op, not error): %v", err)
	}

	var deadlines, opens int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM deadline WHERE notification_id = $1`, p.intimationID).Scan(&deadlines); err != nil {
		t.Fatalf("count deadline: %v", err)
	}
	if deadlines != 1 {
		t.Errorf("deadline rows = %d, want 1 (UNIQUE bars the phantom)", deadlines)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM outbox o
		JOIN deadline d ON d.id = o.aggregate_id
		WHERE o.type = $1 AND d.notification_id = $2`,
		deadline.TypeDeadlineOpened, p.intimationID).Scan(&opens); err != nil {
		t.Fatalf("count opens: %v", err)
	}
	if opens != 1 {
		t.Errorf("deadline.opened rows = %d, want 1 (no phantom event)", opens)
	}
}

// DL4: a real NATIONAL holiday on a business day inside the counting window pushes the
// end date forward AND is recorded in holidays_applied (the auditable "por quê"). The
// holiday is inserted for this test and removed afterwards so it never skews other date
// math on the shared DB.
func TestDeadline_Observed_HolidayShiftsAndAudits(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)

	// Wed 2024-03-06 is the 2nd business day after Mon 2024-03-04; making it a national
	// holiday skips it, so the 5th business day slips from 2024-03-11 to 2024-03-12.
	const holiday = "2024-03-06"
	if _, err := pool.Exec(ctx,
		`INSERT INTO holiday (scope, date, name) VALUES ('NATIONAL', $1, 'Feriado Teste 2c')`, holiday); err != nil {
		t.Fatalf("insert holiday: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(),
			`DELETE FROM holiday WHERE scope = 'NATIONAL' AND date = $1 AND name = 'Feriado Teste 2c'`, holiday); err != nil {
			t.Logf("cleanup holiday: %v", err)
		}
	})

	p := seedDeadlineParentsCommitted(ctx, t, pool)
	ev := observedFor(p, uuid.NewString(), "INTIMACAO", "TJSP", "SP", "2024-03-04")
	if err := newDeadlineUC(pool).OnIntimationObserved(ctx, ev); err != nil {
		t.Fatalf("OnIntimationObserved() error = %v", err)
	}

	var endDate string
	var holidaysRaw []byte
	if err := pool.QueryRow(ctx,
		`SELECT end_date::text, holidays_applied FROM deadline WHERE notification_id = $1`, p.intimationID).
		Scan(&endDate, &holidaysRaw); err != nil {
		t.Fatalf("read deadline: %v", err)
	}

	if endDate != "2024-03-12" {
		t.Errorf("end_date = %q, want 2024-03-12 (holiday pushed it one day)", endDate)
	}
	var applied []string
	if err := json.Unmarshal(holidaysRaw, &applied); err != nil {
		t.Fatalf("unmarshal holidays_applied: %v", err)
	}
	if !contains(applied, holiday) {
		t.Errorf("holidays_applied = %v, want it to contain %q", applied, holiday)
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

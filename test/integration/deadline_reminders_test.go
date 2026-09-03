//go:build integration

// Deadline reminders integration tests (slice 4b-ii) — prove the ETA lembrete/vencimento
// path end to end against a REAL Postgres with every migration applied, reusing the
// creation harness (newDeadlineUC, seedDeadlineParentsCommitted, observedFor). They cover:
// (1) a fresh prazo schedules its D-N reminder_check marks + the D+1 missed_check as
// AGENDADO outbox rows (process_at in the future) alongside deadline.opened; (2) a fired
// reminder re-checks status and emits deadline.due_soon only for an active prazo; (3) the
// carência auto-marks MISSED only a still-OPEN, overdue prazo.
//
// These drive the real use case directly (real repo + calendar + outbox + dedup + uow),
// not the asynq listener — the ETA→asynq hop is covered by outbox_process_at_test.go and
// the handler is a thin decode+delegate covered by unit tests. Each test uses a fresh
// tenant, so counts are isolated on the shared DB.
package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jusassessoria/platform/internal/deadline"
	"github.com/jusassessoria/platform/lib/events"
)

// DR1: a fresh prazo whose vencimento is comfortably in the future schedules three
// deadline.reminder_check marks (days_left 3/1/0, each with process_at = start-of-day(end)
// − days_left) plus one deadline.missed_check (process_at = end + 1 day) — all AGENDADO
// (process_at NOT NULL) and committed in the same tx as deadline.opened.
func TestDeadline_Observed_SchedulesReminderAndMissedChecks(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)
	p := seedDeadlineParentsCommitted(ctx, t, pool)

	// A start ~20 days out keeps every mark (incl. D-3) in the future at birth.
	start := time.Now().UTC().AddDate(0, 0, 20).Format(time.DateOnly)
	ev := observedFor(p, uuid.NewString(), "INTIMACAO", "TJSP", "SP", start)
	if err := newDeadlineUC(pool).OnIntimationObserved(ctx, ev); err != nil {
		t.Fatalf("OnIntimationObserved() error = %v", err)
	}

	var deadlineID string
	var endDate time.Time
	if err := pool.QueryRow(ctx,
		`SELECT id, end_date FROM deadline WHERE notification_id = $1`, p.intimationID).
		Scan(&deadlineID, &endDate); err != nil {
		t.Fatalf("read deadline: %v", err)
	}
	endDate = endDate.UTC()

	// The three reminder_check marks, keyed by days_left, each AGENDADO at the right ETA.
	rows, err := pool.Query(ctx, `
		SELECT (payload->>'days_left')::int, process_at
		FROM outbox
		WHERE type = $1 AND aggregate_type = 'deadline' AND aggregate_id = $2`,
		deadline.TypeDeadlineReminderCheck, deadlineID)
	if err != nil {
		t.Fatalf("query reminder_check rows: %v", err)
	}
	defer rows.Close()

	gotAt := map[int]time.Time{}
	for rows.Next() {
		var daysLeft int
		var at *time.Time
		if err := rows.Scan(&daysLeft, &at); err != nil {
			t.Fatalf("scan reminder_check row: %v", err)
		}
		if at == nil {
			t.Fatalf("reminder_check days_left=%d has NULL process_at (must be AGENDADO)", daysLeft)
		}
		gotAt[daysLeft] = at.UTC()
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate reminder_check rows: %v", err)
	}
	if len(gotAt) != 3 {
		t.Fatalf("reminder_check marks = %d, want 3 (days_left 3/1/0)", len(gotAt))
	}
	for _, daysLeft := range []int{3, 1, 0} {
		want := endDate.AddDate(0, 0, -daysLeft)
		if got, ok := gotAt[daysLeft]; !ok || !got.Equal(want) {
			t.Errorf("reminder days_left=%d process_at = %v (ok=%v), want %v", daysLeft, got, ok, want)
		}
	}

	// One missed_check at end + 1 day, AGENDADO.
	var missedAt *time.Time
	if err := pool.QueryRow(ctx, `
		SELECT process_at FROM outbox
		WHERE type = $1 AND aggregate_id = $2`,
		deadline.TypeDeadlineMissedCheck, deadlineID).Scan(&missedAt); err != nil {
		t.Fatalf("read missed_check: %v", err)
	}
	if missedAt == nil {
		t.Fatal("missed_check has NULL process_at (must be AGENDADO)")
	}
	if want := endDate.AddDate(0, 0, 1); !missedAt.UTC().Equal(want) {
		t.Errorf("missed_check process_at = %v, want %v", missedAt.UTC(), want)
	}

	// deadline.opened is still emitted (immediate — process_at NULL).
	var openedProcessAt *time.Time
	if err := pool.QueryRow(ctx, `
		SELECT process_at FROM outbox WHERE type = $1 AND aggregate_id = $2`,
		deadline.TypeDeadlineOpened, deadlineID).Scan(&openedProcessAt); err != nil {
		t.Fatalf("read deadline.opened: %v", err)
	}
	if openedProcessAt != nil {
		t.Errorf("deadline.opened process_at = %v, want NULL (immediate)", openedProcessAt)
	}
}

// DR2: a fired reminder_check on a still-active (PENDING or OPEN — either is non-terminal)
// prazo emits exactly one deadline.due_soon — IMMEDIATE (process_at NULL) — carrying the
// days_left through. Uses the default seletiva policy, so this fixture is actually born OPEN
// (this fix); OnReminderCheck treats PENDING and OPEN identically (both "active"), so which
// one it is does not matter here — see TestOnReminderCheck_ReChecksStatus (unit) for both.
func TestDeadline_ReminderCheck_ActiveEmitsDueSoon(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)
	p := seedDeadlineParentsCommitted(ctx, t, pool)
	uc := newDeadlineUC(pool)

	start := time.Now().UTC().AddDate(0, 0, 20).Format(time.DateOnly)
	obs := observedFor(p, uuid.NewString(), "INTIMACAO", "TJSP", "SP", start)
	if err := uc.OnIntimationObserved(ctx, obs); err != nil {
		t.Fatalf("OnIntimationObserved() error = %v", err)
	}

	var deadlineID string
	if err := pool.QueryRow(ctx,
		`SELECT id FROM deadline WHERE notification_id = $1`, p.intimationID).Scan(&deadlineID); err != nil {
		t.Fatalf("read deadline id: %v", err)
	}

	mark := deadline.DeadlineReminderCheck{
		Base:       events.Base{EventID: uuid.NewString(), Aggregate: deadlineID},
		TenantID:   p.tenantID.String(),
		DeadlineID: deadlineID,
		DaysLeft:   3,
	}
	if err := uc.OnReminderCheck(ctx, mark); err != nil {
		t.Fatalf("OnReminderCheck() error = %v", err)
	}

	var daysLeft int
	var processAt *time.Time
	if err := pool.QueryRow(ctx, `
		SELECT (payload->>'days_left')::int, process_at
		FROM outbox WHERE type = $1 AND aggregate_id = $2`,
		deadline.TypeDeadlineDueSoon, deadlineID).Scan(&daysLeft, &processAt); err != nil {
		t.Fatalf("read deadline.due_soon: %v", err)
	}
	if daysLeft != 3 {
		t.Errorf("due_soon days_left = %d, want 3", daysLeft)
	}
	if processAt != nil {
		t.Errorf("due_soon process_at = %v, want NULL (immediate)", processAt)
	}
}

// DR3: a fired reminder_check on a CANCELLED prazo emits NOTHING — the re-check no disparo
// suppresses the obsolete lembrete.
func TestDeadline_ReminderCheck_CancelledNoOp(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)
	p := seedDeadlineParentsCommitted(ctx, t, pool)
	uc := newDeadlineUC(pool)

	start := time.Now().UTC().AddDate(0, 0, 20).Format(time.DateOnly)
	obs := observedFor(p, uuid.NewString(), "INTIMACAO", "TJSP", "SP", start)
	if err := uc.OnIntimationObserved(ctx, obs); err != nil {
		t.Fatalf("OnIntimationObserved() error = %v", err)
	}
	if err := uc.OnIntimationCancelled(ctx, cancelledFor(p, uuid.NewString(), "retificada")); err != nil {
		t.Fatalf("OnIntimationCancelled() error = %v", err)
	}

	var deadlineID string
	if err := pool.QueryRow(ctx,
		`SELECT id FROM deadline WHERE notification_id = $1`, p.intimationID).Scan(&deadlineID); err != nil {
		t.Fatalf("read deadline id: %v", err)
	}

	mark := deadline.DeadlineReminderCheck{
		Base:       events.Base{EventID: uuid.NewString(), Aggregate: deadlineID},
		TenantID:   p.tenantID.String(),
		DeadlineID: deadlineID,
		DaysLeft:   1,
	}
	if err := uc.OnReminderCheck(ctx, mark); err != nil {
		t.Fatalf("OnReminderCheck() error = %v (want nil no-op)", err)
	}

	var dueSoon int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM outbox WHERE type = $1 AND aggregate_id = $2`,
		deadline.TypeDeadlineDueSoon, deadlineID).Scan(&dueSoon); err != nil {
		t.Fatalf("count due_soon: %v", err)
	}
	if dueSoon != 0 {
		t.Errorf("deadline.due_soon rows = %d, want 0 (cancelled prazo sends no lembrete)", dueSoon)
	}
}

// DR4: the carência (missed_check) auto-marks a still-OPEN, overdue prazo MISSED and emits
// exactly one deadline.missed. The born prazo is PENDING with a past vencimento; the test
// promotes it to OPEN directly (confirmation is a future slice) to reach the auto-miss path.
func TestDeadline_MissedCheck_OpenOverdueMarksMissed(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)
	p := seedDeadlineParentsCommitted(ctx, t, pool)
	uc := newDeadlineUC(pool)

	// A past start → the born prazo's end_date is already overdue (2024).
	obs := observedFor(p, uuid.NewString(), "INTIMACAO", "TJSP", "SP", "2024-03-04")
	if err := uc.OnIntimationObserved(ctx, obs); err != nil {
		t.Fatalf("OnIntimationObserved() error = %v", err)
	}

	var deadlineID string
	if err := pool.QueryRow(ctx,
		`SELECT id FROM deadline WHERE notification_id = $1`, p.intimationID).Scan(&deadlineID); err != nil {
		t.Fatalf("read deadline id: %v", err)
	}
	// Promote PENDING → OPEN (the F2 confirmation this slice does not own).
	if _, err := pool.Exec(ctx,
		`UPDATE deadline SET status = 'OPEN' WHERE id = $1`, deadlineID); err != nil {
		t.Fatalf("promote to OPEN: %v", err)
	}

	mark := deadline.DeadlineMissedCheck{
		Base:       events.Base{EventID: uuid.NewString(), Aggregate: deadlineID},
		TenantID:   p.tenantID.String(),
		DeadlineID: deadlineID,
	}
	if err := uc.OnMissedCheck(ctx, mark); err != nil {
		t.Fatalf("OnMissedCheck() error = %v", err)
	}

	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM deadline WHERE id = $1`, deadlineID).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "MISSED" {
		t.Errorf("status = %q, want MISSED", status)
	}

	var missed int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM outbox
		WHERE type = $1 AND aggregate_type = 'deadline' AND aggregate_id = $2`,
		deadline.TypeDeadlineMissed, deadlineID).Scan(&missed); err != nil {
		t.Fatalf("count deadline.missed: %v", err)
	}
	if missed != 1 {
		t.Errorf("deadline.missed rows = %d, want 1", missed)
	}
}

// DR5: the carência on a still-PENDING (unconfirmed) prazo is a pure no-op — a prazo that
// was never confirmed is never auto-missed (decisão travada: MISSED SÓ em OPEN). Seeds an
// obrigatoria policy so the prazo is DETERMINISTICALLY born PENDING (not the default
// seletiva's auto-OPEN — see TestDeadline_Observed_DerivesOpenDeadlineAndEvent), because this
// test's whole point is proving the carência guard on a genuinely-PENDING row.
func TestDeadline_MissedCheck_PendingNoOp(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)
	p := seedDeadlineParentsCommitted(ctx, t, pool)
	seedObrigatoriaPolicy(ctx, t, pool, p.tenantID)
	uc := newDeadlineUCAt(pool, deadlineTestNow)

	obs := observedFor(p, uuid.NewString(), "INTIMACAO", "TJSP", "SP", "2024-03-04")
	if err := uc.OnIntimationObserved(ctx, obs); err != nil {
		t.Fatalf("OnIntimationObserved() error = %v", err)
	}

	var deadlineID string
	if err := pool.QueryRow(ctx,
		`SELECT id FROM deadline WHERE notification_id = $1`, p.intimationID).Scan(&deadlineID); err != nil {
		t.Fatalf("read deadline id: %v", err)
	}

	mark := deadline.DeadlineMissedCheck{
		Base:       events.Base{EventID: uuid.NewString(), Aggregate: deadlineID},
		TenantID:   p.tenantID.String(),
		DeadlineID: deadlineID,
	}
	if err := uc.OnMissedCheck(ctx, mark); err != nil {
		t.Fatalf("OnMissedCheck() error = %v (want nil no-op)", err)
	}

	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM deadline WHERE id = $1`, deadlineID).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "PENDING" {
		t.Errorf("status = %q, want PENDING (a PENDING prazo is never auto-missed)", status)
	}

	var missed int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM outbox WHERE type = $1 AND aggregate_id = $2`,
		deadline.TypeDeadlineMissed, deadlineID).Scan(&missed); err != nil {
		t.Fatalf("count deadline.missed: %v", err)
	}
	if missed != 0 {
		t.Errorf("deadline.missed rows = %d, want 0 (no auto-miss on PENDING)", missed)
	}
}

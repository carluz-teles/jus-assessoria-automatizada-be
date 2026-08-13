//go:build integration

// Deadline adjust + manual-transition integration tests — prove the F2 ajuste (PATCH
// /v1/prazos/:id) and the manual met/missed transitions (slice 5c, docs/erd-prazos.md §9)
// end to end against a REAL Postgres: PATCH recomputes end_date through the real judicial
// calendar and commits a deadline.updated; met/missed flip the status and commit a
// deadline.met / deadline.missed — each in ONE tx. They also prove the transition guards
// (only PENDING/OPEN is adjustable; met/missed only from OPEN). These drive the real use case
// (real repo + calendar + outbox + uow), the same composition cmd/api mounts for the routes.
package integration_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/jusassessoria/platform/internal/deadline"
)

// seedOpenDeadline seeds a PENDING prazo via the creation path and confirms it to OPEN,
// returning its id — the starting point for the met/missed transition tests.
func seedOpenDeadline(ctx context.Context, t *testing.T, uc *deadline.UseCase, p deadlineParents) string {
	t.Helper()
	obs := observedFor(p, uuid.NewString(), "INTIMACAO", "TJSP", "SP", "2024-03-04")
	if err := uc.OnIntimationObserved(ctx, obs); err != nil {
		t.Fatalf("OnIntimationObserved() error = %v", err)
	}
	res, err := uc.Confirm(ctx, deadline.ConfirmCommand{
		TenantID:     p.tenantID.String(),
		UserID:       uuid.NewString(),
		IntimationID: p.intimationID.String(),
		Kind:         deadline.KindContestacao,
		Days:         10,
		Counting:     deadline.CountingBusiness,
	})
	if err != nil {
		t.Fatalf("Confirm() error = %v", err)
	}
	return res.Deadline.ID
}

// DLA1: PATCH a still-PENDING prazo's day count → the end_date is RECOMPUTED (2024-03-11 →
// 2024-03-18) through the real calendar, the status stays PENDING (the ajuste never flips the
// lifecycle), and exactly one deadline.updated commits to the outbox with the recomputed end.
func TestDeadline_Adjust_RecomputesEndAndEmitsUpdated(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)
	p := seedDeadlineParentsCommitted(ctx, t, pool)
	uc := newDeadlineUCAt(pool, deadlineTestNow)

	// Seed the PENDING prazo (INTIMACAO/TJSP → MANIFESTACAO/5/BUSINESS → end 2024-03-11).
	obs := observedFor(p, uuid.NewString(), "INTIMACAO", "TJSP", "SP", "2024-03-04")
	if err := uc.OnIntimationObserved(ctx, obs); err != nil {
		t.Fatalf("OnIntimationObserved() error = %v", err)
	}
	var deadlineID, pendingEnd string
	if err := pool.QueryRow(ctx,
		`SELECT id, end_date::text FROM deadline WHERE notification_id = $1`, p.intimationID).
		Scan(&deadlineID, &pendingEnd); err != nil {
		t.Fatalf("read pending deadline: %v", err)
	}
	if pendingEnd != "2024-03-11" {
		t.Fatalf("pending end_date = %q, want 2024-03-11", pendingEnd)
	}

	// PATCH only the day count: 5 → 10 business days.
	days := 10
	res, err := uc.Adjust(ctx, deadline.AdjustCommand{
		TenantID:   p.tenantID.String(),
		UserID:     uuid.NewString(),
		DeadlineID: deadlineID,
		Days:       &days,
	})
	if err != nil {
		t.Fatalf("Adjust() error = %v", err)
	}
	if res.Days != 10 || res.Status != deadline.StatusPending {
		t.Errorf("result days/status = %d/%q, want 10/PENDING", res.Days, res.Status)
	}

	// The row carries the recomputed end + the new day count; status unchanged (still PENDING).
	var status, endDate string
	var days2 int
	if err := pool.QueryRow(ctx,
		`SELECT status, days, end_date::text FROM deadline WHERE id = $1`, deadlineID).
		Scan(&status, &days2, &endDate); err != nil {
		t.Fatalf("read adjusted deadline: %v", err)
	}
	if status != "PENDING" || days2 != 10 || endDate != "2024-03-18" {
		t.Errorf("status/days/end = %q/%d/%q, want PENDING/10/2024-03-18 (recomputed)", status, days2, endDate)
	}

	// Exactly one deadline.updated (aggregate = deadline id), payload PENDING + recomputed end.
	var updEnd, updStatus string
	if err := pool.QueryRow(ctx, `
		SELECT payload->>'end_date', payload->>'status'
		FROM outbox WHERE type = $1 AND aggregate_type = 'deadline' AND aggregate_id = $2`,
		deadline.TypeDeadlineUpdated, deadlineID).Scan(&updEnd, &updStatus); err != nil {
		t.Fatalf("read deadline.updated payload: %v", err)
	}
	if updEnd != "2024-03-18" || updStatus != "PENDING" {
		t.Errorf("deadline.updated end/status = %q/%q, want 2024-03-18/PENDING", updEnd, updStatus)
	}
}

// DLA2: mark an OPEN prazo cumprido → status flips to MET and exactly one deadline.met commits.
func TestDeadline_MarkMet_FlipsMetAndEmits(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)
	p := seedDeadlineParentsCommitted(ctx, t, pool)
	uc := newDeadlineUC(pool)
	deadlineID := seedOpenDeadline(ctx, t, uc, p)

	res, err := uc.MarkMet(ctx, p.tenantID.String(), deadlineID)
	if err != nil {
		t.Fatalf("MarkMet() error = %v", err)
	}
	if res.Status != deadline.StatusMet {
		t.Errorf("result status = %q, want MET", res.Status)
	}

	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM deadline WHERE id = $1`, deadlineID).Scan(&status); err != nil {
		t.Fatalf("read deadline: %v", err)
	}
	if status != "MET" {
		t.Errorf("status = %q, want MET", status)
	}

	var metCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM outbox
		WHERE type = $1 AND aggregate_type = 'deadline' AND aggregate_id = $2`,
		deadline.TypeDeadlineMet, deadlineID).Scan(&metCount); err != nil {
		t.Fatalf("count deadline.met: %v", err)
	}
	if metCount != 1 {
		t.Errorf("deadline.met rows = %d, want 1", metCount)
	}
}

// DLA3: mark an OPEN prazo perdido → status flips to MISSED and exactly one deadline.missed
// commits (the SAME event the D+1 carência auto-miss emits — 4b-ii).
func TestDeadline_MarkMissed_FlipsMissedAndEmits(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)
	p := seedDeadlineParentsCommitted(ctx, t, pool)
	uc := newDeadlineUC(pool)
	deadlineID := seedOpenDeadline(ctx, t, uc, p)

	res, err := uc.MarkMissed(ctx, p.tenantID.String(), deadlineID)
	if err != nil {
		t.Fatalf("MarkMissed() error = %v", err)
	}
	if res.Status != deadline.StatusMissed {
		t.Errorf("result status = %q, want MISSED", res.Status)
	}

	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM deadline WHERE id = $1`, deadlineID).Scan(&status); err != nil {
		t.Fatalf("read deadline: %v", err)
	}
	if status != "MISSED" {
		t.Errorf("status = %q, want MISSED", status)
	}

	var missedCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM outbox
		WHERE type = $1 AND aggregate_type = 'deadline' AND aggregate_id = $2`,
		deadline.TypeDeadlineMissed, deadlineID).Scan(&missedCount); err != nil {
		t.Fatalf("count deadline.missed: %v", err)
	}
	if missedCount != 1 {
		t.Errorf("deadline.missed rows = %d, want 1", missedCount)
	}
}

// DLA4: the transition guards against a REAL row. met on a still-PENDING prazo is refused
// (ErrDeadlineNotOpen, status untouched); after a met flip, the prazo is terminal and PATCH is
// refused (ErrDeadlineNotAdjustable, dates frozen).
func TestDeadline_TransitionGuards(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)
	p := seedDeadlineParentsCommitted(ctx, t, pool)
	uc := newDeadlineUCAt(pool, deadlineTestNow)

	// A PENDING prazo (never confirmed) cannot be marked met.
	obs := observedFor(p, uuid.NewString(), "INTIMACAO", "TJSP", "SP", "2024-03-04")
	if err := uc.OnIntimationObserved(ctx, obs); err != nil {
		t.Fatalf("OnIntimationObserved() error = %v", err)
	}
	var deadlineID string
	if err := pool.QueryRow(ctx,
		`SELECT id FROM deadline WHERE notification_id = $1`, p.intimationID).Scan(&deadlineID); err != nil {
		t.Fatalf("read deadline: %v", err)
	}
	if _, err := uc.MarkMet(ctx, p.tenantID.String(), deadlineID); err == nil {
		t.Fatal("MarkMet() on a PENDING prazo error = nil, want ErrDeadlineNotOpen")
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM deadline WHERE id = $1`, deadlineID).Scan(&status); err != nil {
		t.Fatalf("read deadline: %v", err)
	}
	if status != "PENDING" {
		t.Errorf("status after refused met = %q, want PENDING (untouched)", status)
	}

	// Confirm → OPEN, then met → MET; now the prazo is terminal and a PATCH is refused.
	if _, err := uc.Confirm(ctx, deadline.ConfirmCommand{
		TenantID: p.tenantID.String(), UserID: uuid.NewString(), IntimationID: p.intimationID.String(),
		Kind: deadline.KindContestacao, Days: 10, Counting: deadline.CountingBusiness,
	}); err != nil {
		t.Fatalf("Confirm() error = %v", err)
	}
	if _, err := uc.MarkMet(ctx, p.tenantID.String(), deadlineID); err != nil {
		t.Fatalf("MarkMet() error = %v", err)
	}
	days := 20
	if _, err := uc.Adjust(ctx, deadline.AdjustCommand{
		TenantID: p.tenantID.String(), DeadlineID: deadlineID, Days: &days,
	}); err == nil {
		t.Fatal("Adjust() on a MET prazo error = nil, want ErrDeadlineNotAdjustable")
	}
	var days2 int
	if err := pool.QueryRow(ctx, `SELECT days FROM deadline WHERE id = $1`, deadlineID).Scan(&days2); err != nil {
		t.Fatalf("read deadline: %v", err)
	}
	if days2 != 10 {
		t.Errorf("days after refused adjust = %d, want 10 (frozen)", days2)
	}
}

//go:build integration

// Deadline schema integration tests — prove, against a REAL Postgres with every
// migration applied, that migration 0024 shaped the prazos subsystem as the
// catalog (docs/erd-modelo-de-dados.md) and docs/erd-prazos.md §4/§8 describe:
//   - deadline grew the audit/product deltas (tenant_id, kind, source,
//     confirmed_by/at, doubled_reason, rules_version);
//   - deadline.status is now a closed set defaulting to PENDING — a derived prazo
//     is born PENDING and only becomes OPEN on the human F2 confirmation (2c);
//   - deadline_rule (the versioned, seeded rules layer, §8) exists and ships the
//     safe v0 conservative rules;
//   - task (the actionable N-per-prazo work item, §4) exists and is insertable.
//
// These are pure schema assertions — no slice logic. The container's owner role
// bypasses RLS (no FORCE ROW LEVEL SECURITY), so the seeds below insert directly;
// every write happens inside a tx that is rolled back, leaving the shared DB clean.
package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestDeadlineSchema_Deltas asserts the seven columns that migration 0024 adds to
// deadline all exist, via information_schema (the schema is the contract the 2c
// slice's sqlc models are generated from).
func TestDeadlineSchema_Deltas(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)

	const colQuery = `SELECT EXISTS (
		SELECT 1 FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'deadline' AND column_name = $1
	)`
	for _, col := range []string{
		"tenant_id", "kind", "source", "confirmed_by",
		"confirmed_at", "doubled_reason", "rules_version",
	} {
		t.Run(col, func(t *testing.T) {
			var exists bool
			if err := pool.QueryRow(ctx, colQuery, col).Scan(&exists); err != nil {
				t.Fatalf("checking column deadline.%s: %v", col, err)
			}
			if !exists {
				t.Errorf("expected column deadline.%s to exist after 0024", col)
			}
		})
	}
}

// TestDeadlineSchema_StatusCheck proves the status enum is enforced by the DB: a
// derived prazo may be born 'PENDING', but an out-of-set value is rejected by the
// CHECK. Each insert runs in its own rolled-back tx over a freshly seeded parent
// chain (tenant → case → record → intimation), so only the status column varies.
func TestDeadlineSchema_StatusCheck(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)

	tests := []struct {
		name    string
		status  string
		wantErr bool
	}{
		{name: "PENDING accepted (the born state)", status: "PENDING", wantErr: false},
		{name: "BOGUS rejected by CHECK", status: "BOGUS", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer tx.Rollback(ctx)

			p := seedDeadlineParents(ctx, t, tx)

			_, err = tx.Exec(ctx, `
				INSERT INTO deadline
					(tenant_id, court_record_id, notification_id,
					 start_date, end_date, days, counting, status)
				VALUES ($1, $2, $3, DATE '2024-01-16', DATE '2024-02-06', 15, 'BUSINESS', $4)`,
				p.tenantID, p.courtRecordID, p.intimationID, tt.status)

			if tt.wantErr && err == nil {
				t.Fatalf("status=%q: expected the CHECK to reject the insert, got nil", tt.status)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("status=%q: expected the insert to succeed, got: %v", tt.status, err)
			}
		})
	}
}

// TestDeadlineRule_Seeded asserts the rules table exists and ships at least the
// safe v0 conservative rules (docs/erd-prazos.md §8): the seed is the whole point
// of the table in this slice — without it the 2c resolver has nothing to match.
func TestDeadlineRule_Seeded(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM deadline_rule WHERE rules_version = 'v0'`).Scan(&n); err != nil {
		t.Fatalf("counting v0 deadline_rule seed: %v", err)
	}
	if n < 3 {
		t.Errorf("expected >= 3 seeded v0 rules, got %d", n)
	}
}

// TestTask_Insertable proves the new task table exists and accepts a minimal row
// (only its NOT NULLs: tenant_id, title, source) with status defaulting to OPEN.
// The write is rolled back so the shared DB stays clean.
func TestTask_Insertable(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)

	tenantID := seedDeadlineTenant(ctx, t, tx)

	var status string
	if err := tx.QueryRow(ctx, `
		INSERT INTO task (tenant_id, title, source)
		VALUES ($1, 'Contestar', 'RULE')
		RETURNING status`, tenantID).Scan(&status); err != nil {
		t.Fatalf("inserting minimal task: %v", err)
	}
	if status != "OPEN" {
		t.Errorf("expected task.status to default to OPEN, got %q", status)
	}
}

// deadlineParents holds the ids of the parent chain a deadline row needs (FKs).
type deadlineParents struct {
	tenantID      uuid.UUID
	courtRecordID uuid.UUID
	intimationID  uuid.UUID
}

// seedDeadlineParents inserts a tenant → court_case → court_record → intimation
// chain inside the caller's tx and returns the ids a deadline row anchors on. The
// caller rolls the tx back; nothing persists.
func seedDeadlineParents(ctx context.Context, t *testing.T, tx pgx.Tx) deadlineParents {
	t.Helper()

	tenantID := seedDeadlineTenant(ctx, t, tx)

	var caseID uuid.UUID
	if err := tx.QueryRow(ctx,
		`INSERT INTO court_case (tenant_id) VALUES ($1) RETURNING id`,
		tenantID).Scan(&caseID); err != nil {
		t.Fatalf("seeding court_case: %v", err)
	}

	cnj := "0000009-99.2024.8.26." + uuid.NewString()[:4]
	var recordID uuid.UUID
	if err := tx.QueryRow(ctx, `
		INSERT INTO court_record (tenant_id, case_id, cnj_number, degree, court)
		VALUES ($1, $2, $3, 'G1', 'TJSP') RETURNING id`,
		tenantID, caseID, cnj).Scan(&recordID); err != nil {
		t.Fatalf("seeding court_record: %v", err)
	}

	made := time.Date(2024, 1, 12, 0, 0, 0, 0, time.UTC)
	var intimationID uuid.UUID
	if err := tx.QueryRow(ctx, `
		INSERT INTO intimation
			(tenant_id, case_id, court_record_id, hash,
			 made_available_at, published_at, deadline_start_at, content, source)
		VALUES ($1, $2, $3, $4, $5, $5, $5, 'teor', 'DJEN') RETURNING id`,
		tenantID, caseID, recordID, uuid.NewString(), made).Scan(&intimationID); err != nil {
		t.Fatalf("seeding intimation: %v", err)
	}

	return deadlineParents{tenantID: tenantID, courtRecordID: recordID, intimationID: intimationID}
}

// seedDeadlineTenant inserts a throwaway tenant in the caller's tx and returns its id.
func seedDeadlineTenant(ctx context.Context, t *testing.T, tx pgx.Tx) uuid.UUID {
	t.Helper()

	var tenantID uuid.UUID
	if err := tx.QueryRow(ctx,
		`INSERT INTO tenant (clerk_org_id, name) VALUES ($1, 'Deadline Schema Test') RETURNING id`,
		"org_"+uuid.NewString()).Scan(&tenantID); err != nil {
		t.Fatalf("seeding tenant: %v", err)
	}
	return tenantID
}

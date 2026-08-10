//go:build integration

// Entitlement gating integration tests (fatia 5-A) — prove FindOrCreateCourtRecord's
// billing gate against a real Postgres: the new CountActiveCourtRecordsByTenant query
// runs in the caller's tx, a brand-new record (MISS) is refused with the typed
// ErrProcessLimitReached once the tenant's ACTIVE count meets active_process_limit
// (creating nothing), and a reobservation (HIT) is never gated even at the ceiling.
package integration_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jusassessoria/platform/internal/acquisition"
	"github.com/jusassessoria/platform/lib/database"
)

// findOrCreateCourtRecord runs one FindOrCreateCourtRecord for cnj/G1 in its own
// tenant-scoped tx (RLS applied by the UoW exactly as production does), returning the
// record or the typed error the gate raised.
func findOrCreateCourtRecord(t *testing.T, pool *pgxpool.Pool, tenantID, cnj string, limit int) (*acquisition.CourtRecord, error) {
	t.Helper()
	repo := acquisition.NewRepository(pool)
	uow := database.NewUnitOfWork(pool)

	var got *acquisition.CourtRecord
	err := uow.Do(context.Background(), tenantID, func(tx database.Tx) error {
		cr, _, ferr := repo.FindOrCreateCourtRecord(context.Background(), tx, acquisition.FindOrCreateCourtRecordParams{
			TenantID:           tenantID,
			CNJNumber:          cnj,
			Degree:             acquisition.DegreeG1,
			Court:              "TJSP",
			Completeness:       0.5,
			NextSyncAt:         time.Now().Add(24 * time.Hour),
			ActiveProcessLimit: limit,
		})
		got = cr
		return ferr
	})
	return got, err
}

// countActiveCourtRecords counts the tenant's ACTIVE court records as the owner (RLS
// bypassed) — the ground truth the gate is asserted against.
func countActiveCourtRecords(t *testing.T, pool *pgxpool.Pool, tenantID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM court_record WHERE tenant_id=$1 AND lifecycle='ACTIVE'`, tenantID).Scan(&n); err != nil {
		t.Fatalf("count active court_record: %v", err)
	}
	return n
}

// AC1: below the limit, each brand-new record is created and counts against the
// ceiling — two distinct CNJs under a limit of 2 both land.
func TestGating_BelowLimit_CreatesRecords(t *testing.T) {
	pool := newPool(t)
	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-gate-below", 0)

	if _, err := findOrCreateCourtRecord(t, pool, tenantID, "0000001-11.2024.8.26.0100", 2); err != nil {
		t.Fatalf("first create under limit: %v", err)
	}
	if _, err := findOrCreateCourtRecord(t, pool, tenantID, "0000002-22.2024.8.26.0100", 2); err != nil {
		t.Fatalf("second create under limit: %v", err)
	}
	if n := countActiveCourtRecords(t, pool, tenantID); n != 2 {
		t.Fatalf("active court records = %d, want 2", n)
	}
}

// AC2: at the limit, a brand-new record (MISS) is refused with the typed
// ErrProcessLimitReached and NOTHING is created — the count holds at the ceiling.
func TestGating_AtLimit_BlocksNewRecord(t *testing.T) {
	pool := newPool(t)
	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-gate-at", 0)

	// Fill the single slot the plan allows.
	if _, err := findOrCreateCourtRecord(t, pool, tenantID, "0000001-11.2024.8.26.0100", 1); err != nil {
		t.Fatalf("seed the one allowed record: %v", err)
	}

	// A brand-new CNJ now exceeds the ceiling.
	_, err := findOrCreateCourtRecord(t, pool, tenantID, "0000002-22.2024.8.26.0100", 1)
	if !errors.Is(err, acquisition.ErrProcessLimitReached) {
		t.Fatalf("error = %v, want ErrProcessLimitReached", err)
	}
	if n := countActiveCourtRecords(t, pool, tenantID); n != 1 {
		t.Fatalf("active court records = %d, want 1 (the blocked record must not be created)", n)
	}
}

// AC3: a tenant treated as limit 0 (what the adapter resolves for a tenant with no
// subscription) has EVERY first new record blocked — nothing is ever created.
func TestGating_ZeroLimit_BlocksEverything(t *testing.T) {
	pool := newPool(t)
	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-gate-zero", 0)

	_, err := findOrCreateCourtRecord(t, pool, tenantID, "0000001-11.2024.8.26.0100", 0)
	if !errors.Is(err, acquisition.ErrProcessLimitReached) {
		t.Fatalf("error = %v, want ErrProcessLimitReached under a zero limit", err)
	}
	if n := countActiveCourtRecords(t, pool, tenantID); n != 0 {
		t.Fatalf("active court records = %d, want 0 (fail-closed blocks the first record)", n)
	}
}

// AC4: a reobservation (HIT — the same CNJ+degree already exists) is NEVER gated,
// even when the tenant is at or over its limit: it re-marks the existing record and
// creates no new row.
func TestGating_Reobservation_NotGated(t *testing.T) {
	pool := newPool(t)
	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-gate-hit", 0)

	const cnj = "0000001-11.2024.8.26.0100"
	// Create the record while a slot is free.
	first, err := findOrCreateCourtRecord(t, pool, tenantID, cnj, 1)
	if err != nil {
		t.Fatalf("initial create: %v", err)
	}

	// Reobserve the SAME record with the ceiling now full (limit 0): it must pass.
	again, err := findOrCreateCourtRecord(t, pool, tenantID, cnj, 0)
	if err != nil {
		t.Fatalf("reobservation under a full ceiling was gated: %v", err)
	}
	if again.ID != first.ID {
		t.Fatalf("reobservation resolved a different record: got %s, want %s", again.ID, first.ID)
	}
	if n := countActiveCourtRecords(t, pool, tenantID); n != 1 {
		t.Fatalf("active court records = %d, want 1 (reobservation must not create a second row)", n)
	}
}

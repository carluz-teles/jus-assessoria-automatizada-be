//go:build integration

package integration_test

import (
	"context"
	"testing"

	"github.com/jusassessoria/platform/lib/database"
)

// TestMigrations_UpIsIdempotentAndCreatesSchema runs the app's own migrate
// runner (lib/database.Up) a SECOND time against the already-migrated container:
// it must be a no-op (the runner treats migrate.ErrNoChange as success). Then it
// asserts a representative table from three concerns exists. This reuses the
// production migrate path — the test never reimplements migrations.
func TestMigrations_UpIsIdempotentAndCreatesSchema(t *testing.T) {
	ctx := context.Background()

	// TestMain already applied the schema. A second Up must return nil.
	if err := database.Up(ctx, connString); err != nil {
		t.Fatalf("second Up() must be a no-op, got: %v", err)
	}

	pool := newPool(t)

	// One table from each of three concerns: identity (tenant), consolidation
	// (court_record) and the infra outbox (outbox).
	const existsQuery = `SELECT EXISTS (
		SELECT 1 FROM information_schema.tables
		WHERE table_schema = 'public' AND table_name = $1
	)`
	for _, table := range []string{"tenant", "court_record", "outbox"} {
		t.Run(table, func(t *testing.T) {
			var exists bool
			if err := pool.QueryRow(ctx, existsQuery, table).Scan(&exists); err != nil {
				t.Fatalf("checking table %q: %v", table, err)
			}
			if !exists {
				t.Errorf("expected table %q to exist after migration", table)
			}
		})
	}
}

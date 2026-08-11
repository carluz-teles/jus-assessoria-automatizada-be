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

	// One table from each concern: identity (tenant), consolidation (court_record),
	// the infra outbox (outbox) and the national DJEN firehose (publication).
	const existsQuery = `SELECT EXISTS (
		SELECT 1 FROM information_schema.tables
		WHERE table_schema = 'public' AND table_name = $1
	)`
	for _, table := range []string{"tenant", "court_record", "outbox", "publication", "watched_oab"} {
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

	// The publication store is only useful with its access-path indexes: the hash
	// unique (dedup / ON CONFLICT target), the GIN over recipient_oabs (the local OAB
	// match) and the made_available_at btree (retention prune + onboarding window).
	t.Run("publication_indexes", func(t *testing.T) {
		const idxQuery = `SELECT EXISTS (
			SELECT 1 FROM pg_indexes WHERE tablename = 'publication' AND indexname = $1
		)`
		for _, idx := range []string{
			"publication_hash_key",
			"publication_recipient_oabs_gin",
			"publication_made_available_at_idx",
		} {
			var exists bool
			if err := pool.QueryRow(ctx, idxQuery, idx).Scan(&exists); err != nil {
				t.Fatalf("checking index %q: %v", idx, err)
			}
			if !exists {
				t.Errorf("expected index %q on publication after migration", idx)
			}
		}
	})
}

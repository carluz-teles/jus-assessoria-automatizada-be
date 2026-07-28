//go:build integration

// Package integration_test proves, against a REAL Postgres, what the unit tests
// with a mocked repo cannot: that the embedded migrations apply cleanly and are
// idempotent, and that the Row-Level Security policies actually isolate one
// tenant from another (docs/erd-backend.md §5c, §4d.4).
//
// The `integration` build tag keeps this out of the plain green gate — `go test
// ./...` skips this package; only `go test -tags=integration ./...` runs it,
// which spins a throwaway Postgres via testcontainers-go.
package integration_test

import (
	"context"
	"log"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/jusassessoria/platform/lib/database"
)

// connString is the DSN of the throwaway Postgres started once for the whole
// package in TestMain. Each test opens its own pool against it.
var connString string

// TestMain boots a single pgvector container, applies the migrations once, and
// runs every test against it. One container amortizes the seconds of image
// startup across all tests; the migrated schema is shared and read-mostly.
func TestMain(m *testing.M) {
	ctx := context.Background()

	container, err := postgres.Run(ctx,
		"pgvector/pgvector:pg16",
		postgres.WithDatabase("jus"),
		postgres.WithUsername("jus"),
		postgres.WithPassword("jus"),
		// The wait strategy is a generic container customizer in this version, not
		// a postgres-module option. Two occurrences: Postgres logs "ready" once
		// during its init bootstrap and again when it finally accepts connections.
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(90*time.Second),
		),
	)
	if err != nil {
		log.Fatalf("starting postgres container: %v", err)
	}

	// terminate is the single teardown path — invoked on every early exit below
	// and once after the suite. Terminate errors are logged, not fatal: the
	// container is ephemeral regardless.
	terminate := func() {
		if err := container.Terminate(ctx); err != nil {
			log.Printf("terminating container: %v", err)
		}
	}

	connString, err = container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		terminate()
		log.Fatalf("resolving connection string: %v", err)
	}

	// Apply the schema once up front via the app's own migrate runner (reused,
	// not reimplemented) so every test sees a migrated database. A second Up
	// being a no-op is asserted by TestMigrations, not here.
	if err := database.Up(ctx, connString); err != nil {
		terminate()
		log.Fatalf("applying migrations: %v", err)
	}

	code := m.Run()
	terminate()
	os.Exit(code)
}

// newPool opens a pgxpool against the shared container and closes it on cleanup.
func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	pool, err := pgxpool.New(context.Background(), connString)
	if err != nil {
		t.Fatalf("opening pool: %v", err)
	}
	t.Cleanup(pool.Close)

	return pool
}

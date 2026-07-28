//go:build integration

package integration_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jusassessoria/platform/lib/database"
)

// TestUnitOfWork_CommitAndRollback exercises lib/database.UnitOfWork against the
// real database. The mock-based unit tests prove the control flow; this proves
// the same flow survives real pgx/Postgres: a committed write persists, and a
// write in a unit of work whose fn returns an error leaves no trace.
func TestUnitOfWork_CommitAndRollback(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)
	uow := database.NewUnitOfWork(pool)

	// A tenant to hang the app_users off of. Inserted as the owner (RLS bypassed).
	tenantID := uuid.NewString()
	if _, err := pool.Exec(ctx,
		`INSERT INTO tenant (id, clerk_org_id, name) VALUES ($1, $2, $2)`,
		tenantID, "org-uow",
	); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}

	// Commit path: the unit of work returns nil, so the insert is committed.
	const committedID = "uow-committed-user"
	err := uow.Do(ctx, tenantID, func(tx database.Tx) error {
		_, e := tx.Exec(ctx,
			`INSERT INTO app_user (clerk_user_id, tenant_id, email) VALUES ($1, $2, $3)`,
			committedID, tenantID, "commit@uow.test",
		)
		return e
	})
	if err != nil {
		t.Fatalf("commit Do: %v", err)
	}
	if got := countByClerkID(t, pool, committedID); got != 1 {
		t.Fatalf("after commit, app_user count = %d, want 1", got)
	}

	// Rollback path: fn inserts then returns an error, so the tx rolls back and
	// the row must not exist. The domain error must reach the caller unwrapped.
	wantErr := errors.New("forced rollback")
	const rolledBackID = "uow-rolledback-user"
	err = uow.Do(ctx, tenantID, func(tx database.Tx) error {
		if _, e := tx.Exec(ctx,
			`INSERT INTO app_user (clerk_user_id, tenant_id, email) VALUES ($1, $2, $3)`,
			rolledBackID, tenantID, "rollback@uow.test",
		); e != nil {
			return e
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("rollback Do error = %v, want %v", err, wantErr)
	}
	if got := countByClerkID(t, pool, rolledBackID); got != 0 {
		t.Fatalf("after rollback, app_user count = %d, want 0", got)
	}
}

// countByClerkID counts matching app_user rows as the owner (RLS bypassed) so it
// sees the row regardless of any tenant scope.
func countByClerkID(t *testing.T, pool *pgxpool.Pool, clerkID string) int {
	t.Helper()

	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM app_user WHERE clerk_user_id = $1`, clerkID,
	).Scan(&n); err != nil {
		t.Fatalf("count by clerk id: %v", err)
	}

	return n
}

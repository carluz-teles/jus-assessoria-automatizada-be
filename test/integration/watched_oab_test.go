//go:build integration

package integration_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/jusassessoria/platform/internal/acquisition"
	"github.com/jusassessoria/platform/lib/database"
)

// TestReplaceWatchedOABs drives the watched-OAB index against real Postgres: a
// tenant's set is populated in its tenant tx, and a later replace (a scope change)
// clears the old set and lands the new one — so a removed OAB stops matching. The
// keys are the normalized "NUMBER|UF" the national match joins against.
func TestReplaceWatchedOABs(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-woab", 0)
	integrationID := seedIntegration(t, pool, tenantID, "DJEN")

	repo := acquisition.NewRepository(pool)
	uow := database.NewUnitOfWork(pool)
	replace := func(keys ...string) {
		t.Helper()
		if err := uow.Do(ctx, tenantID, func(tx database.Tx) error {
			return repo.ReplaceWatchedOABs(ctx, tx, tenantID, integrationID, keys)
		}); err != nil {
			t.Fatalf("ReplaceWatchedOABs(%v): %v", keys, err)
		}
	}

	keysOf := func() []string {
		t.Helper()
		rows, err := pool.Query(ctx,
			`SELECT oab_key FROM watched_oab WHERE integration_id = $1 ORDER BY oab_key`, integrationID)
		if err != nil {
			t.Fatalf("query watched_oab: %v", err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var k string
			if err := rows.Scan(&k); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out = append(out, k)
		}
		return out
	}

	// First populate: two OABs land (idempotent — a repeat is a no-op).
	replace("347019|SP", "198988|MG")
	replace("347019|SP", "198988|MG")
	if got := keysOf(); len(got) != 2 || got[0] != "198988|MG" || got[1] != "347019|SP" {
		t.Fatalf("after populate = %v, want [198988|MG 347019|SP]", got)
	}

	// Scope change: the old set is cleared and only the new key remains.
	replace("321511|SP")
	if got := keysOf(); len(got) != 1 || got[0] != "321511|SP" {
		t.Fatalf("after scope change = %v, want [321511|SP]", got)
	}
}

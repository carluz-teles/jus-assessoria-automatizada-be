//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jusassessoria/platform/internal/acquisition"
	"github.com/jusassessoria/platform/lib/database"
)

// TestMatchPublicationsByDay proves the national join: a publication landed in the
// firehose matches ONLY the tenant whose watched OAB is one of its recipients. Two
// tenants watch different OABs; the publication carries tenant A's OAB, so the
// system-level match returns A (once) and not B. Test-unique OAB keys + day isolate
// it from other tests' rows in the shared schema.
func TestMatchPublicationsByDay(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)
	repo := acquisition.NewRepository(pool)
	uow := database.NewUnitOfWork(pool)

	const (
		keyA = "MATCH-A-347019|SP" // watched by tenant A + recipient of the publication
		keyB = "MATCH-B-999999|RJ" // watched by tenant B, never a recipient
		keyX = "MATCH-X-198988|MG" // a co-recipient nobody watches
	)
	day := time.Date(2020, 1, 15, 0, 0, 0, 0, time.UTC)

	tenantA, tenantB := uuid.NewString(), uuid.NewString()
	seedTenant(t, pool, tenantA, "org-match-a", 0)
	seedTenant(t, pool, tenantB, "org-match-b", 0)
	integA := seedIntegration(t, pool, tenantA, "DJEN")
	integB := seedIntegration(t, pool, tenantB, "DJEN")

	watch := func(tenantID, integID, key string) {
		t.Helper()
		if err := uow.Do(ctx, tenantID, func(tx database.Tx) error {
			return repo.ReplaceWatchedOABs(ctx, tx, tenantID, integID, []string{key})
		}); err != nil {
			t.Fatalf("watch %s: %v", key, err)
		}
	}
	watch(tenantA, integA, keyA)
	watch(tenantB, integB, keyB)

	// Land one national publication whose recipients are keyA (A's) + keyX (unwatched).
	if err := uow.Do(ctx, "", func(tx database.Tx) error {
		_, err := repo.InsertPublications(ctx, tx, []acquisition.PublicationParams{{
			Hash: "match-test-pub", Court: "TJSP", CNJNumber: "1",
			MadeAvailableAt: day, RecipientOABs: []string{keyA, keyX},
			Payload: json.RawMessage(`{"hash":"match-test-pub"}`),
		}})
		return err
	}); err != nil {
		t.Fatalf("insert publication: %v", err)
	}

	// The system-level match returns only tenant A, once.
	var matches []acquisition.PublicationMatch
	if err := uow.DoSystem(ctx, func(tx database.Tx) error {
		var derr error
		matches, derr = repo.MatchPublicationsByDay(ctx, tx, day)
		return derr
	}); err != nil {
		t.Fatalf("MatchPublicationsByDay: %v", err)
	}

	if len(matches) != 1 {
		t.Fatalf("got %d matches, want 1 (only tenant A watches a recipient)", len(matches))
	}
	if matches[0].TenantID != tenantA || matches[0].OABKey != keyA {
		t.Errorf("match = {tenant:%s oab:%s}, want {%s %s}", matches[0].TenantID, matches[0].OABKey, tenantA, keyA)
	}
}

//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jusassessoria/platform/internal/acquisition"
	"github.com/jusassessoria/platform/lib/database"
)

// TestInsertPublications_LandsAndDedups drives the national firehose write against a
// real Postgres: a batch lands (tallied as new), the row keeps its court / recipient
// OABs / raw payload, and a re-ingest of the same hashes (the daily lookback) is a
// no-op — only genuinely new hashes count. The store is national, so the write runs
// in a system-level tx (empty tenant), like the relay.
func TestInsertPublications_LandsAndDedups(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)
	repo := acquisition.NewRepository(pool)
	uow := database.NewUnitOfWork(pool)

	day := time.Date(2025, 8, 8, 0, 0, 0, 0, time.UTC)
	pubA := acquisition.PublicationParams{
		Hash: "pub-test-A", Court: "TJSP", CNJNumber: "10755763920248260002",
		MadeAvailableAt: day, RecipientOABs: []string{"347019|SP", "198988|MG"},
		Payload: json.RawMessage(`{"hash":"pub-test-A","siglaTribunal":"TJSP"}`),
	}
	pubB := acquisition.PublicationParams{
		Hash: "pub-test-B", Court: "TRT15", CNJNumber: "10077117520258260224",
		MadeAvailableAt: day, RecipientOABs: []string{"321511|SP"},
		Payload: json.RawMessage(`{"hash":"pub-test-B","siglaTribunal":"TRT15"}`),
	}
	pubC := acquisition.PublicationParams{
		Hash: "pub-test-C", Court: "TRE-SP", CNJNumber: "00000000000000000000",
		MadeAvailableAt: day, RecipientOABs: nil, // no advogado recipients → empty set
		Payload: json.RawMessage(`{"hash":"pub-test-C"}`),
	}

	insert := func(params ...acquisition.PublicationParams) int {
		t.Helper()
		var n int
		if err := uow.Do(ctx, "", func(tx database.Tx) error {
			var derr error
			n, derr = repo.InsertPublications(ctx, tx, params)
			return derr
		}); err != nil {
			t.Fatalf("InsertPublications: %v", err)
		}
		return n
	}

	// First landing: both new.
	if n := insert(pubA, pubB); n != 2 {
		t.Fatalf("first insert newCount = %d, want 2", n)
	}

	// The row kept its columns.
	var court string
	var oabs []string
	if err := pool.QueryRow(ctx,
		`SELECT court, recipient_oabs FROM publication WHERE hash = $1`, pubA.Hash).
		Scan(&court, &oabs); err != nil {
		t.Fatalf("read publication A: %v", err)
	}
	if court != "TJSP" {
		t.Errorf("court = %q, want TJSP", court)
	}
	if len(oabs) != 2 || oabs[0] != "347019|SP" {
		t.Errorf("recipient_oabs = %v, want [347019|SP 198988|MG]", oabs)
	}

	// Re-ingest A+B (lookback) plus a new C: only C is new (ON CONFLICT DO NOTHING).
	if n := insert(pubA, pubB, pubC); n != 1 {
		t.Fatalf("re-insert newCount = %d, want 1 (only C new)", n)
	}

	// A no-recipient item still lands, with an empty (non-null) array.
	if err := pool.QueryRow(ctx,
		`SELECT recipient_oabs FROM publication WHERE hash = $1`, pubC.Hash).Scan(&oabs); err != nil {
		t.Fatalf("read publication C: %v", err)
	}
	if oabs == nil || len(oabs) != 0 {
		t.Errorf("C recipient_oabs = %v, want empty non-null array", oabs)
	}

	// Exactly three rows for this test's prefix — no phantom duplicates.
	var total int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM publication WHERE hash LIKE 'pub-test-%'`).Scan(&total); err != nil {
		t.Fatalf("count publications: %v", err)
	}
	if total != 3 {
		t.Fatalf("total publications = %d, want 3", total)
	}
}

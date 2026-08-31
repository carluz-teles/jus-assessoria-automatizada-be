package indexing

import (
	"context"
	"testing"

	"github.com/pashagolub/pgxmock/v4"
)

// retrieval_test.go asserts the SearchChunks SQL shape + args against pgxmock (no real Postgres):
// the tenant filter is bound, the query vector + topK reach the query, the optional
// courtRecordID binds NULL (whole-tenant) vs the id (single-process), and the rows map to
// ChunkHits ordered as returned.

var searchColumns = []string{"document_id", "page", "text", "score", "title", "document_type"}

func TestSearchChunks_WholeTenant(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool(pgxmock.QueryMatcherOption(pgxmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	// courtRecordID nil → the third arg is a nil (SQL NULL) — the $3 IS NULL branch keeps the
	// whole-tenant corpus in scope. Args: vector, tenant, nil, topK.
	mock.ExpectQuery(searchChunksSQL).
		WithArgs(pgxmock.AnyArg(), testTenant, nil, 5).
		WillReturnRows(
			pgxmock.NewRows(searchColumns).
				AddRow(testDoc, 3, "primeiro trecho", 0.91, "Petição inicial", "PET").
				AddRow(testDoc, 7, "segundo trecho", 0.80, "Contrato social", "CONTRSOCIAL"),
		)

	hits, err := SearchChunks(context.Background(), SearchDeps{Pool: mock}, testTenant, nil, []float32{0.1, 0.2, 0.3, 0.4}, 5)
	if err != nil {
		t.Fatalf("SearchChunks: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("hits = %d, want 2", len(hits))
	}
	if hits[0].Page != 3 || hits[0].Score < 0.90 || hits[0].DocumentID != testDoc {
		t.Errorf("hit[0] wrong: %+v", hits[0])
	}
	if hits[1].Text != "segundo trecho" {
		t.Errorf("hit[1] text = %q", hits[1].Text)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestSearchChunks_ScopedToProcess(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool(pgxmock.QueryMatcherOption(pgxmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	crid := "33333333-3333-3333-3333-333333333333"
	// A non-nil courtRecordID binds as the third arg (the $3 = court_record_id branch scopes to
	// one process).
	mock.ExpectQuery(searchChunksSQL).
		WithArgs(pgxmock.AnyArg(), testTenant, crid, 3).
		WillReturnRows(pgxmock.NewRows(searchColumns).AddRow(testDoc, 1, "trecho", 0.5, "Petição inicial", "PET"))

	hits, err := SearchChunks(context.Background(), SearchDeps{Pool: mock}, testTenant, &crid, []float32{1, 0, 0, 0}, 3)
	if err != nil {
		t.Fatalf("SearchChunks: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits = %d, want 1", len(hits))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestSearchChunks_EmptyCourtRecordIDIsWholeTenant(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool(pgxmock.QueryMatcherOption(pgxmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	empty := ""
	// A pointer to "" is treated as absent → SQL NULL (whole-tenant), same as nil.
	mock.ExpectQuery(searchChunksSQL).
		WithArgs(pgxmock.AnyArg(), testTenant, nil, 10).
		WillReturnRows(pgxmock.NewRows(searchColumns))

	hits, err := SearchChunks(context.Background(), SearchDeps{Pool: mock}, testTenant, &empty, []float32{1, 2, 3, 4}, 10)
	if err != nil {
		t.Fatalf("SearchChunks: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("hits = %d, want 0", len(hits))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

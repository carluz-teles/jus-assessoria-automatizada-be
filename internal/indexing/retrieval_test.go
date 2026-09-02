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
		WithArgs(pgxmock.AnyArg(), testTenant, nil, 20).
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
		WithArgs(pgxmock.AnyArg(), testTenant, crid, 12).
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

// TestSearchChunks_SimilarityFloorFiltersNoise — hits below minSimilarity are dropped as noise,
// hits at/above are kept. Two rows straddle the floor (one above, one below) → only the good one
// survives.
func TestSearchChunks_SimilarityFloorFiltersNoise(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool(pgxmock.QueryMatcherOption(pgxmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	mock.ExpectQuery(searchChunksSQL).
		WithArgs(pgxmock.AnyArg(), testTenant, nil, 20).
		WillReturnRows(
			pgxmock.NewRows(searchColumns).
				AddRow(testDoc, 1, "relevante", minSimilarity+0.10, "Petição inicial", "PET").
				AddRow(testDoc, 2, "ruído", minSimilarity-0.10, "Aleatório", "OUTRO"),
		)

	hits, err := SearchChunks(context.Background(), SearchDeps{Pool: mock}, testTenant, nil, []float32{0.1, 0.2, 0.3, 0.4}, 5)
	if err != nil {
		t.Fatalf("SearchChunks: %v", err)
	}
	if len(hits) != 1 || hits[0].Text != "relevante" {
		t.Fatalf("hits = %+v, want only the above-floor hit", hits)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestSearchChunks_FloorDegradesToBestHit — when EVERY hit is below the floor (weak corpus), the
// graceful-degrade path keeps the single best (first, most-similar) hit rather than returning
// empty, so a marginal-but-real chunk still grounds the caller.
func TestSearchChunks_FloorDegradesToBestHit(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool(pgxmock.QueryMatcherOption(pgxmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	// Both below the floor; the first is the best (rows arrive distance-ascending).
	mock.ExpectQuery(searchChunksSQL).
		WithArgs(pgxmock.AnyArg(), testTenant, nil, 20).
		WillReturnRows(
			pgxmock.NewRows(searchColumns).
				AddRow(testDoc, 1, "melhor fraco", minSimilarity-0.05, "Petição inicial", "PET").
				AddRow(testDoc, 2, "pior fraco", minSimilarity-0.20, "Outro", "OUTRO"),
		)

	hits, err := SearchChunks(context.Background(), SearchDeps{Pool: mock}, testTenant, nil, []float32{0.1, 0.2, 0.3, 0.4}, 5)
	if err != nil {
		t.Fatalf("SearchChunks: %v", err)
	}
	if len(hits) != 1 || hits[0].Text != "melhor fraco" {
		t.Fatalf("hits = %+v, want single best-hit degrade", hits)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestIsLikelyBrokenText(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		text string
		want bool
	}{
		{
			name: "normal legal prose is not broken",
			text: "A pretensão autoral está baseada em suposto contrato de prestação de serviços " +
				"firmado entre as partes, cuja existência a ré nega categoricamente nos autos.",
			want: false,
		},
		{
			name: "broken extraction (word-shattered OCR) is broken",
			text: "A pretens ão a ut o ra l est á ba sead a em su po st o co ntra to de prest aç ão " +
				"de serv iç os Nã o há inc lus iv e no s tic kets qua lquer tipo de va lo r co bra doou",
			want: true,
		},
		{
			name: "short text is inconclusive → not broken",
			text: "art. 5º, II, CF",
			want: false,
		},
		{
			name: "substantial text with many numbers is not broken",
			text: "O valor da causa é de R$ 1.250,00 conforme processo 0001234-56.2024.8.26.0100 " +
				"distribuído em 15/03/2024 às 14:30, totalizando 12 parcelas de 104,17 cada mês.",
			want: false,
		},
		{
			name: "empty text is not broken",
			text: "",
			want: false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isLikelyBrokenText(tc.text); got != tc.want {
				t.Errorf("isLikelyBrokenText(%q) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}

// TestSearchChunks_FiltersBrokenAndOverFetches — a broken chunk that ranks ABOVE the floor is
// dropped by the quality gate (never reaches the LLM), leaving the real chunks. topK=2 over-fetches
// (LIMIT 8) so the drop doesn't starve the result.
func TestSearchChunks_FiltersBrokenChunks(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool(pgxmock.QueryMatcherOption(pgxmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	broken := "A pretens ão a ut o ra l est á ba sead a em su po st o co ntra to de prest aç ão de serv iç os"
	good := "A pretensão autoral está baseada em suposto contrato de prestação de serviços firmado entre as partes."

	// topK=2 → over-fetch LIMIT 8. Broken chunk scores highest but must be dropped.
	mock.ExpectQuery(searchChunksSQL).
		WithArgs(pgxmock.AnyArg(), testTenant, nil, 8).
		WillReturnRows(
			pgxmock.NewRows(searchColumns).
				AddRow(testDoc, 1, broken, 0.95, "Petição inicial", "PET").
				AddRow(testDoc, 2, good, 0.90, "Petição inicial", "PET"),
		)

	hits, err := SearchChunks(context.Background(), SearchDeps{Pool: mock}, testTenant, nil, []float32{0.1, 0.2, 0.3, 0.4}, 2)
	if err != nil {
		t.Fatalf("SearchChunks: %v", err)
	}
	if len(hits) != 1 || hits[0].Text != good {
		t.Fatalf("hits = %+v, want only the non-broken chunk", hits)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestSearchChunks_AllBrokenReturnsEmpty — if EVERY candidate is broken, return empty (ungrounded)
// rather than degrading to a broken chunk. Grounding with garbage is worse than ungrounded.
func TestSearchChunks_AllBrokenReturnsEmpty(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool(pgxmock.QueryMatcherOption(pgxmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	broken1 := "A pretens ão a ut o ra l est á ba sead a em su po st o co ntra to de prest aç ão de serv iç os"
	broken2 := "Nã o há inc lus iv e no s tic kets qua lquer tipo de va lo r co bra doou pel a par te ré"

	mock.ExpectQuery(searchChunksSQL).
		WithArgs(pgxmock.AnyArg(), testTenant, nil, 20).
		WillReturnRows(
			pgxmock.NewRows(searchColumns).
				AddRow(testDoc, 1, broken1, 0.95, "Petição inicial", "PET").
				AddRow(testDoc, 2, broken2, 0.92, "Petição inicial", "PET"),
		)

	hits, err := SearchChunks(context.Background(), SearchDeps{Pool: mock}, testTenant, nil, []float32{0.1, 0.2, 0.3, 0.4}, 5)
	if err != nil {
		t.Fatalf("SearchChunks: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("hits = %+v, want empty (no broken fallback)", hits)
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
		WithArgs(pgxmock.AnyArg(), testTenant, nil, 40).
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

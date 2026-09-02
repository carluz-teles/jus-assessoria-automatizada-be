package indexing

import (
	"context"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	pgvector "github.com/pgvector/pgvector-go"

	"github.com/jusassessoria/platform/lib/database"
)

// retrieval.go is the READ path of the fatia — the cosine-similarity search over chunks the
// advisory consumes later. It is exported (SearchChunks) but has NO HTTP handler: it is called
// in-process by the advisory slice. Tenant isolation is load-bearing here — a chunk leaked
// across tenants is leaked autos — so the query JOINs document and filters document.tenant_id.

// querier is the sliver of a pgx pool SearchChunks needs: a single Query. Depending on it (not
// *pgxpool.Pool) keeps the read unit-testable with a fake that asserts the SQL + args.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// SearchDeps carries what SearchChunks needs: the read pool. It is separate from the pipeline's
// Deps because retrieval is a read on the pool, not a write in a tenant tx — the tenant filter is
// an explicit WHERE (the caller is the advisory, which passes the tenant it already verified).
type SearchDeps struct {
	Pool querier
}

// ChunkHit is one retrieval result: the document + page the chunk came from, its text, and the
// cosine SIMILARITY score (1 - distance, so higher = closer). Ordered by distance ascending
// (most similar first) and capped at topK.
type ChunkHit struct {
	DocumentID string
	Page       int
	Text       string
	Score      float64
	// DocumentTitle/DocumentType come from the JOINed document row — enough for a
	// caller to attribute a retrieved chunk to a human-readable source (e.g.
	// draft.SuggestTheses labels a thesis's evidence with the autos document it
	// came from) without a second query.
	DocumentTitle string
	DocumentType  string
}

// searchChunksSQL ranks chunks by cosine distance to the query vector (embedding <=> $1, the
// vector_cosine_ops the 0034 HNSW index accelerates). It JOINs document to enforce tenant
// isolation (document.tenant_id = $2 — a chunk carries no tenant_id, so this is the ONLY barrier)
// and optionally scopes to one process (document.court_record_id = $3 when $3 is non-null).
// Soft-deleted documents are excluded. Score is 1 - distance (cosine similarity). $4 is topK.
const searchChunksSQL = `SELECT c.document_id, c.page, c.text, 1 - (c.embedding <=> $1) AS score, COALESCE(d.title, '') AS title, COALESCE(d.document_type, '') AS document_type
	FROM chunk c
	JOIN document d ON d.id = c.document_id
	WHERE d.tenant_id = $2
	  AND d.deleted_at IS NULL
	  AND ($3::uuid IS NULL OR d.court_record_id = $3::uuid)
	ORDER BY c.embedding <=> $1
	LIMIT $4`

// minSimilarity is the similarity FLOOR for a chunk to count as relevant grounding. ChunkHit.Score
// is cosine SIMILARITY (1 - distance): 1.0 = identical direction, 0 = orthogonal, so HIGHER is
// closer and the floor filters hits scoring BELOW it. voyage-3.5-lite similarities for genuinely
// on-topic autos chunks sit well above this; the 0.30 floor is deliberately conservative — it
// drops the "5 least-bad" noise a weak/empty corpus returns without cutting legitimate grounding
// (a too-aggressive floor would starve the RAG of real evidence). Tune up only with recall data.
const minSimilarity = 0.30

// Quality gate for broken-PDF-extraction chunks. Scanned/image PDFs whose OCR/text-extraction
// picotou as palavras produce garbage like "A pretens ão a ut o ra l est á ba sead a" — tokens
// mostly 1-2 chars, spaces in the wrong places. Those chunks embed and rank like any other, so a
// similar broken chunk can win retrieval and be REPRODUCED verbatim by the LLM (in theses AND in
// the drafted peça). We can't re-index the 139k corpus now, so we filter at the retrieval edge:
// broken text NEVER reaches the LLM. Grounding with garbage is worse than ungrounded.

const (
	// minAvgTokenLen is the average token length (non-space chars ÷ token count) below which a
	// substantial chunk is judged broken. Calibration: normal legal prose sits ~5 chars/token;
	// the broken extraction sits ~2 (words shattered into 1-2 char fragments). Measured on the
	// live corpus: ~19.889 / 139.813 chunks (~14%) sit below 3.2, and those are the garbage. 3.0
	// is a conservative floor — comfortably below normal prose, above the ~2 of the garbage, so it
	// doesn't clip legitimate dense text (numbers, abbreviations) that a tighter bar might.
	minAvgTokenLen = 3.0

	// brokenMinChars / brokenMinTokens gate WHICH chunks we judge. Short chunks are inconclusive —
	// a legitimate heading or a short citation ("art. 5º, II") has few tokens and a low average by
	// nature, so scoring it would false-positive. We only judge substantial chunks and let short
	// ones pass (isLikelyBrokenText → false) rather than risk discarding real short grounding.
	brokenMinChars  = 60
	brokenMinTokens = 15

	// maxSingleCharFrac reinforces the average: even when the average squeaks above the floor, a
	// chunk where a large share of tokens are single characters is shattered text. Normal prose
	// almost never exceeds this; broken extraction blows past it.
	maxSingleCharFrac = 0.35
)

// isLikelyBrokenText reports whether text is probably broken PDF extraction (word-shattered OCR)
// rather than real prose. It is deliberately conservative — a false negative merely lets a
// marginal chunk through, but a false positive would silently drop legitimate grounding.
//
// Only SUBSTANTIAL chunks are judged (>= brokenMinChars and >= brokenMinTokens); anything shorter
// is inconclusive and returns false. For substantial chunks the primary signal is the average
// token length (non-space chars ÷ tokens): below minAvgTokenLen means the words are shattered.
// A secondary signal — the fraction of single-char tokens exceeding maxSingleCharFrac — catches
// shattered chunks whose average happens to sneak just above the floor.
func isLikelyBrokenText(text string) bool {
	tokens := strings.Fields(text)
	if len(tokens) < brokenMinTokens {
		return false // too few tokens to judge — inconclusive, keep it.
	}

	nonSpaceChars := 0
	singleCharTokens := 0
	for _, tok := range tokens {
		n := len([]rune(tok))
		nonSpaceChars += n
		if n == 1 {
			singleCharTokens++
		}
	}
	if nonSpaceChars < brokenMinChars {
		return false // too little content to judge — inconclusive, keep it.
	}

	avgTokenLen := float64(nonSpaceChars) / float64(len(tokens))
	if avgTokenLen < minAvgTokenLen {
		return true
	}

	singleCharFrac := float64(singleCharTokens) / float64(len(tokens))
	return singleCharFrac > maxSingleCharFrac
}

// overFetchMultiplier / overFetchCap size the internal SQL LIMIT so that, after dropping broken and
// below-floor candidates in Go, we still have enough survivors to fill topK. We fetch more than
// topK because the broken/noise chunks would otherwise eat into the returned set.
const (
	overFetchMultiplier = 4
	overFetchCap        = 40
)

// overFetchLimit returns how many candidates to pull from SQL for a given topK: topK * multiplier,
// but never more than topK + cap (so a large topK doesn't explode the scan).
func overFetchLimit(topK int) int {
	limit := topK * overFetchMultiplier
	if capped := topK + overFetchCap; limit > capped {
		limit = capped
	}
	return limit
}

// SearchChunks returns the topK chunks most similar (cosine) to query within tenantID, optionally
// scoped to courtRecordID (nil = across the tenant's whole corpus). Tenant isolation is enforced
// in SQL (the JOIN + document.tenant_id filter), so a caller cannot read another tenant's autos
// even by passing a foreign courtRecordID. The query vector binds through pgvector.Vector; a nil
// courtRecordID binds SQL NULL (the $3 IS NULL branch keeps the whole corpus in scope).
func SearchChunks(ctx context.Context, deps SearchDeps, tenantID string, courtRecordID *string, query []float32, topK int) ([]ChunkHit, error) {
	// A nil/empty courtRecordID → SQL NULL (whole-tenant scope); a set one binds as the uuid the
	// $3::uuid cast parses. any(nil) marshals to NULL through pgx.
	var crid any
	if courtRecordID != nil && *courtRecordID != "" {
		crid = *courtRecordID
	}

	// Over-fetch: pull more candidates than topK so the Go-side quality/similarity filters
	// (broken-text drop + minSimilarity floor) have room to discard noise and still return topK
	// real hits. The SQL LIMIT is the over-fetch size; the final truncation to topK is in Go.
	rows, err := deps.Pool.Query(ctx, searchChunksSQL,
		pgvector.NewVector(query),
		tenantID,
		crid,
		overFetchLimit(topK),
	)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	defer rows.Close()

	var hits []ChunkHit
	for rows.Next() {
		var h ChunkHit
		if err := rows.Scan(&h.DocumentID, &h.Page, &h.Text, &h.Score, &h.DocumentTitle, &h.DocumentType); err != nil {
			return nil, database.WrapInfra(err)
		}
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		return nil, database.WrapInfra(err)
	}

	// Two-stage filter over the over-fetched candidates. Rows arrive ordered by distance ascending
	// (most similar first).
	//
	// Stage 1 — DROP broken-PDF-extraction chunks unconditionally: word-shattered OCR garbage must
	// NEVER reach the LLM, because it gets reproduced verbatim in theses and in the drafted peça.
	// This is not a "prefer" — grounding with garbage is strictly worse than ungrounded.
	//
	// Stage 2 — apply the similarity floor to the surviving (non-broken) hits, then truncate to
	// topK. Degrade with grace: if the floor drops everything but non-broken candidates existed,
	// keep the single best NON-BROKEN hit rather than returning empty. If EVERY candidate was
	// broken, return empty (ungrounded) — we deliberately do not fall back to a broken chunk.
	nonBroken := make([]ChunkHit, 0, len(hits))
	for _, h := range hits {
		if !isLikelyBrokenText(h.Text) {
			nonBroken = append(nonBroken, h)
		}
	}
	// Preserve most-similar-first ordering (already sorted by SQL, but be explicit after filtering).
	sort.SliceStable(nonBroken, func(i, j int) bool { return nonBroken[i].Score > nonBroken[j].Score })

	kept := make([]ChunkHit, 0, topK)
	for _, h := range nonBroken {
		if h.Score >= minSimilarity {
			kept = append(kept, h)
			if len(kept) == topK {
				break
			}
		}
	}
	if len(kept) == 0 && len(nonBroken) > 0 {
		return nonBroken[:1], nil // degrade-to-best, but only among non-broken candidates.
	}
	return kept, nil
}

package draft

// rag.go — shared RAG helper for the draft slice.
//
// runRAG is a package-level function (not a method) shared by GenerateUseCase
// and ChatUseCase, as both pipelines require the same embed → SearchChunks flow.
// It is intentionally free of use-case state so it can be tested independently.
//
// Degradation contract (non-fatal in both callers):
//   - emb nil OR search.Pool nil → (nil, nil, false) — no grounding, caller proceeds.
//   - Embed error → warn + (nil, nil, false).
//   - SearchChunks error → warn + (nil, nil, false).
//   - Empty hits → (nil, nil, false).
//
// Grounding = len(hits) > 0.

import (
	"context"
	"log/slog"

	"github.com/jusassessoria/platform/internal/indexing"
)

// runRAG embeds queryText and searches the top topK chunks, optionally scoped to
// courtRecordID (nil → whole-tenant). Returns the chunk texts (for prompt injection),
// the raw ChunkHit slice (for citation validation in chat), and whether grounding was
// achieved (len(hits) > 0).
//
// It degrades gracefully on any failure — the caller continues ungrounded.
func runRAG(
	ctx context.Context,
	emb embedder,
	search indexing.SearchDeps,
	tenantID string,
	courtRecordID *string,
	queryText string,
	topK int,
) (texts []string, hits []indexing.ChunkHit, grounded bool) {
	if emb == nil || search.Pool == nil {
		return nil, nil, false
	}

	vecs, _, err := emb.Embed(ctx, []string{queryText})
	if err != nil || len(vecs) == 0 {
		slog.WarnContext(ctx, "draft rag: embed failed",
			slog.String("tenant_id", tenantID),
			slog.Any("error", err),
		)
		return nil, nil, false
	}

	h, err := indexing.SearchChunks(ctx, search, tenantID, courtRecordID, vecs[0], topK)
	if err != nil {
		slog.WarnContext(ctx, "draft rag: search chunks failed",
			slog.String("tenant_id", tenantID),
			slog.Any("error", err),
		)
		return nil, nil, false
	}
	if len(h) == 0 {
		return nil, nil, false
	}

	out := make([]string, len(h))
	for i, hit := range h {
		out[i] = hit.Text
	}
	return out, h, true
}

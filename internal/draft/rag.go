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
// Quando `cache` é não-nil, consulta antes de chamar embed/search — hit poupa
// 500ms-1s (Voyage) + 50-200ms (pgvector) por request repetida. Cache nulo =
// comportamento legado (sem cache). Erros de cache degradam pra miss e não
// bloqueiam a request.
//
// Degrada graciosamente em qualquer falha — o caller segue ungrounded.
func runRAG(
	ctx context.Context,
	emb embedder,
	search indexing.SearchDeps,
	cache *RAGCache,
	tenantID string,
	courtRecordID *string,
	queryText string,
	topK int,
) (texts []string, hits []indexing.ChunkHit, grounded bool) {
	// Cache lookup — só quando o cache está montado. Miss ou erro cai no
	// caminho normal abaixo sem branch adicional.
	if cache != nil {
		if t, h, g, ok := cache.Get(ctx, tenantID, courtRecordID, queryText, topK); ok {
			return t, h, g
		}
	}

	if emb == nil || search.Pool == nil {
		return nil, nil, false
	}

	// InputQuery — this is the RETRIEVAL side. Voyage is asymmetric; embedding the query in the
	// query sub-space (not "document") is what makes the search recall the right chunks.
	vecs, _, err := emb.Embed(ctx, []string{queryText}, indexing.InputQuery)
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

	// Só cacheia resultado grounded — miss/degrade não é útil cachear (o
	// próximo request paga o mesmo custo, e se o corpus for indexado no
	// meio-tempo o cache atrapalha).
	if cache != nil {
		cache.Set(ctx, tenantID, courtRecordID, queryText, topK, out, h, true)
	}

	return out, h, true
}

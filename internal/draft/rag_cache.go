package draft

// rag_cache.go — cache Redis do resultado do runRAG. Serve os dois problemas
// de latência num só lugar:
//
//   P1. Voyage embed (500ms-1s por chamada) → não roda quando o cache bate.
//   P2. pgvector search (50-200ms) → também pulada, já que cachearmos o
//       output completo (texts + hits + grounded).
//
// Chave: `rag:v1:{tenant}:{crid}:{topK}:{hex8-sha256(queryText)}`. O hash de
// 8 bytes evita chaves gigantes (o queryText tem centenas de bytes) e o
// prefixo `rag:v1:` permite bump de versão sem colidir com cache antigo. TTL
// default 5min — combina com a Partida ephemeral do FE (advogado troca tipo
// da peça poucas vezes em sequência) e com o fluxo teses→generate na mesma
// sessão. Prod pode subir pra 15-30min sem impacto de qualidade (chunks do
// processo não mudam nesse timeframe).
//
// Miss-safe: qualquer erro de Redis (rede, timeout, unmarshal) degrada
// silenciosamente pra "cache miss" — o runRAG segue chamando embed+search
// como sempre fez. Zero risco de bloquear a request.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jusassessoria/platform/internal/indexing"
	"github.com/redis/go-redis/v9"
)

// RAGCache guarda resultados de runRAG num Redis. Instância única por
// binário; o use case recebe *RAGCache no construtor. Nil = comportamento
// legado (sem cache, direto ao Voyage + pgvector).
type RAGCache struct {
	rdb *redis.Client
	ttl time.Duration
}

// NewRAGCache monta o cache. Se rdb for nil, devolve nil — os use cases
// tratam como "cache desabilitado" sem branch adicional na chamada.
func NewRAGCache(rdb *redis.Client, ttl time.Duration) *RAGCache {
	if rdb == nil {
		return nil
	}
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return &RAGCache{rdb: rdb, ttl: ttl}
}

// ragCacheEntry é o payload serializado no Redis. Grounded vem redundante
// (dá pra derivar de len(Hits) > 0) só pra evitar surpresa na descompressão.
type ragCacheEntry struct {
	Texts    []string            `json:"t"`
	Hits     []indexing.ChunkHit `json:"h"`
	Grounded bool                `json:"g"`
}

// cacheKey monta a chave estável do cache. courtRecordID nulo (RAG
// tenant-wide) vira "*" pra distinguir de crid vazio.
func (c *RAGCache) cacheKey(tenantID string, courtRecordID *string, queryText string, topK int) string {
	crid := "*"
	if courtRecordID != nil {
		crid = *courtRecordID
	}
	h := sha256.Sum256([]byte(queryText))
	return fmt.Sprintf("rag:v1:%s:%s:%d:%x", tenantID, crid, topK, h[:8])
}

// Get busca o resultado cacheado. Retorna ok=false em qualquer condição de
// erro (cache miss, timeout, unmarshal fail) — o caller trata os 4 casos
// idênticos (segue pra Voyage+pgvector). Nunca retorna erro pra não obrigar
// o caller a inflar handling.
func (c *RAGCache) Get(
	ctx context.Context,
	tenantID string,
	courtRecordID *string,
	queryText string,
	topK int,
) (texts []string, hits []indexing.ChunkHit, grounded bool, ok bool) {
	if c == nil {
		return nil, nil, false, false
	}
	key := c.cacheKey(tenantID, courtRecordID, queryText, topK)
	raw, err := c.rdb.Get(ctx, key).Bytes()
	if err != nil {
		if !errors.Is(err, redis.Nil) {
			slog.WarnContext(ctx, "rag cache: get failed", slog.String("key", key), slog.Any("error", err))
		}
		return nil, nil, false, false
	}
	var entry ragCacheEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		slog.WarnContext(ctx, "rag cache: unmarshal failed", slog.String("key", key), slog.Any("error", err))
		return nil, nil, false, false
	}
	return entry.Texts, entry.Hits, entry.Grounded, true
}

// Set grava o resultado no cache com o TTL configurado. Fire-and-forget do
// ponto de vista do caller — erros vão pro log e retornam.
func (c *RAGCache) Set(
	ctx context.Context,
	tenantID string,
	courtRecordID *string,
	queryText string,
	topK int,
	texts []string,
	hits []indexing.ChunkHit,
	grounded bool,
) {
	if c == nil {
		return
	}
	key := c.cacheKey(tenantID, courtRecordID, queryText, topK)
	payload, err := json.Marshal(ragCacheEntry{Texts: texts, Hits: hits, Grounded: grounded})
	if err != nil {
		slog.WarnContext(ctx, "rag cache: marshal failed", slog.String("key", key), slog.Any("error", err))
		return
	}
	if err := c.rdb.Set(ctx, key, payload, c.ttl).Err(); err != nil {
		slog.WarnContext(ctx, "rag cache: set failed", slog.String("key", key), slog.Any("error", err))
	}
}

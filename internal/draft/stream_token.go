package draft

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/redis/go-redis/v9"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/httpx"
)

// stream_token.go — sistema de tokens curtos pro SSE.
//
// Problema: EventSource (HTML5 spec) não aceita headers custom, então o
// FE não consegue mandar `Authorization: Bearer <JWT>`. Query string
// (?token=<JWT>) expõe o JWT do Clerk no histórico do browser e nos logs
// de acesso — inaceitável mesmo com TTL curto do Clerk.
//
// Solução: 2 passos.
//   1. FE (autenticado com Bearer normal) chama POST /v1/pecas/:id/stream-token
//      → recebe {token: "opaco-32-chars", expires_at: iso}
//   2. FE abre EventSource /v1/pecas/:id/generation-stream?stream_token=xxx
//      → middleware específico valida o token opaco no Redis e popula
//        principal.tenant_id sem tocar no Clerk
//
// Escopo do token: tenant + draftID + kind ("generation-stream"). TTL 2min.
// Storage: Redis, key `stream_token:<opaque>` → `<tenant>|<draft>|<kind>`.
// Single-use? Não — o EventSource pode reconectar N vezes durante os 2min
// (útil pra reconexão automática); depois o FE pega token novo se precisar.

const (
	streamTokenTTL    = 2 * time.Minute
	streamTokenKind   = "generation-stream"
	streamTokenPrefix = "stream_token:"
	streamTokenBytes  = 24 // 32 chars em base64url
)

// StreamTokenStore é a porta que o issuer/validator usa pra guardar tokens.
// Redis é o único backend hoje (TTL nativo), mas o interface deixa fake
// nos testes.
type StreamTokenStore interface {
	Set(ctx context.Context, token, value string, ttl time.Duration) error
	Get(ctx context.Context, token string) (string, error)
}

// RedisStreamTokenStore embrulha *redis.Client como StreamTokenStore.
type RedisStreamTokenStore struct {
	c *redis.Client
}

func NewRedisStreamTokenStore(c *redis.Client) *RedisStreamTokenStore {
	return &RedisStreamTokenStore{c: c}
}

func (r *RedisStreamTokenStore) Set(ctx context.Context, token, value string, ttl time.Duration) error {
	return r.c.Set(ctx, streamTokenPrefix+token, value, ttl).Err()
}

func (r *RedisStreamTokenStore) Get(ctx context.Context, token string) (string, error) {
	v, err := r.c.Get(ctx, streamTokenPrefix+token).Result()
	if err == redis.Nil {
		return "", nil
	}
	return v, err
}

// WithStreamTokenStore ativa o endpoint de emissão + o middleware de
// validação. Sem isso, POST /stream-token retorna 501 e o SSE só aceita
// Bearer normal (que na prática o EventSource não consegue mandar).
func (h *Handler) WithStreamTokenStore(store StreamTokenStore) *Handler {
	h.streamTokens = store
	return h
}

// issueStreamToken responde POST /v1/pecas/:id/stream-token. Requer o
// principal autenticado (Bearer JWT normal); gera um token opaco de 32
// chars e persiste no Redis por 2 minutos com escopo (tenant, draft).
func (h *Handler) issueStreamToken(c *fiber.Ctx) error {
	if h.streamTokens == nil {
		return httpx.WriteError(c, apperr.NewInvalid("stream token não habilitado"))
	}
	tenantID := httpx.TenantFromCtx(c)
	draftID := c.Params("id")
	token, err := newOpaqueToken()
	if err != nil {
		return apperr.NewInfra("stream token: rand", err)
	}
	value := tenantID + "|" + draftID + "|" + streamTokenKind
	if err := h.streamTokens.Set(c.UserContext(), token, value, streamTokenTTL); err != nil {
		return apperr.NewInfra("stream token: set", err)
	}
	return c.JSON(fiber.Map{
		"token":      token,
		"expires_in": int(streamTokenTTL.Seconds()),
	})
}

// StreamTokenAuth é o middleware específico pra rotas SSE. Roda ANTES da
// auth normal do /v1: checa se veio ?stream_token=xxx, valida no store, e
// se ok popula o principal (tenant) no ctx e chama next. Se stream_token
// ausente ou inválido, cai pro auth normal (que provavelmente falha 401
// porque EventSource não manda Bearer).
//
// Match: só ativa quando o path termina em "/generation-stream" — outras
// rotas seguem o auth normal.
func StreamTokenAuth(store StreamTokenStore, fallback fiber.Handler) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if !strings.HasSuffix(c.Path(), "/generation-stream") || store == nil {
			return fallback(c)
		}
		token := strings.TrimSpace(c.Query("stream_token"))
		if token == "" {
			return fallback(c)
		}
		value, err := store.Get(c.UserContext(), token)
		if err != nil {
			return apperr.NewInfra("stream token lookup", err)
		}
		if value == "" {
			return httpx.WriteError(c, apperr.NewUnauthorized("stream_token inválido ou expirado"))
		}
		// value = "<tenant>|<draft>|<kind>". Valida escopo do draft na URL.
		parts := strings.Split(value, "|")
		if len(parts) != 3 || parts[2] != streamTokenKind {
			return httpx.WriteError(c, apperr.NewUnauthorized("stream_token com escopo inválido"))
		}
		tenantID, draftID := parts[0], parts[1]
		if urlDraft := c.Params("id"); urlDraft != draftID {
			return httpx.WriteError(c, apperr.NewUnauthorized("stream_token não corresponde ao draft"))
		}
		// Popula principal só com tenant + userID vazio (não temos user id
		// no token; o SSE só precisa do tenant pra derivar saga_state).
		httpx.SetPrincipal(c, httpx.Principal{TenantID: tenantID})
		return c.Next()
	}
}

// newOpaqueToken retorna 24 bytes random encodados em base64url (~32 chars).
// Colisão prática impossível (2^192).
func newOpaqueToken() (string, error) {
	b := make([]byte, streamTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read rand: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

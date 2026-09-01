// Command worker-ai consumes the "ai" asynq queue (LLM calls: summaries,
// classification, extraction). Boot skeleton: same lifecycle as the api —
// config → health → bootstrap → serve — with an empty mux the AI feature slice
// fills in. No migrations (§5b.3).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"

	"github.com/jusassessoria/platform/internal/advisory"
	"github.com/jusassessoria/platform/internal/aiusage"
	"github.com/jusassessoria/platform/internal/draft"
	"github.com/jusassessoria/platform/internal/indexing"
	"github.com/jusassessoria/platform/lib/config"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
	"github.com/jusassessoria/platform/lib/health"
	"github.com/jusassessoria/platform/lib/llm"
	"github.com/jusassessoria/platform/lib/pubsub"
	"github.com/jusassessoria/platform/lib/telemetry"
	"github.com/jusassessoria/platform/pkg/lifecycle"
)

const (
	serviceName = "worker-ai"
	queueName   = "ai"
	// concurrency is low: every AI job costs money per attempt and calls a
	// rate-limited upstream, so few run in parallel. Tune with the docker slice.
	concurrency = 3
)

func main() {
	logger := telemetry.SetupDefault(os.Stdout, config.LogLevelFromEnv())
	if err := run(logger); err != nil {
		logger.Error("worker boot failed", "service", serviceName, "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	ctx := context.Background()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	if err := health.WaitAll(ctx, cfg); err != nil {
		return fmt.Errorf("dependency health check: %w", err)
	}

	telemetryShutdown, err := telemetry.Setup(ctx, cfg, serviceName)
	if err != nil {
		return fmt.Errorf("setup telemetry: %w", err)
	}

	pool, err := database.NewPool(ctx, cfg)
	if err != nil {
		return fmt.Errorf("open database pool: %w", err)
	}

	redisOpt, err := asynq.ParseRedisURI(cfg.RedisURL)
	if err != nil {
		return fmt.Errorf("parse redis uri: %w", err)
	}

	// asynq's own logs go through slog (structured, OTLP); LogLevel=ErrorLevel keeps
	// its per-task retry Warns from duplicating the Observe middleware's failure log.
	srv := asynq.NewServer(redisOpt, asynq.Config{
		Concurrency: concurrency,
		Queues:      map[string]int{queueName: concurrency},
		Logger:      events.NewAsynqLogger(logger),
		LogLevel:    asynq.ErrorLevel,
	})

	// Observe wraps EVERY handler (consumer span + failure log) — all ai listeners
	// inherit the instrumentation for free.
	mux := asynq.NewServeMux()
	mux.Use(events.Observe(logger))

	// ── Draft AI generation listener (Fatia 3) ────────────────────────────────
	// Compose the generation use case with the real dependencies. The LLM generator
	// is optional: when OPENROUTER_API_KEY is not configured, gen is nil and the use
	// case marks every draft FAILED "IA não configurada" (the degraded path). The
	// Voyage embedder is also optional: when absent, the use case degrades to
	// ungrounded generation (no RAG, no citations). Both are soft failures — the
	// worker still boots and the listener is always registered (it handles the
	// "generator nil → FAILED" case itself, per the Architect's design).
	{
		uow := database.NewUnitOfWork(pool)
		outbox := events.NewOutbox()
		repo := draft.NewRepository()
		dedup := draft.NewGenerateDeduper()

		// LLM generator (OpenRouter). nil when the key is absent. The usage recorder
		// (cost/tokens per call, best-effort) is always wired — it degrades to a no-op
		// insert failure (logged, never propagated) if the pool is unreachable.
		var gen llm.Generator
		if cfg.OpenRouterAPIKey != "" {
			httpClient := &http.Client{Timeout: 2 * time.Minute}
			g, err := llm.NewOpenRouterGenerator(
				cfg.OpenRouterAPIKey,
				cfg.OpenRouterBaseURL,
				cfg.OpenRouterModel,
				httpClient,
				aiusage.NewRecorder(pool),
			)
			if err != nil {
				return fmt.Errorf("init openrouter generator: %w", err)
			}
			gen = g
			logger.Info("AI generation: OpenRouter generator configured", "model", cfg.OpenRouterModel)
		} else {
			logger.Warn("OPENROUTER_API_KEY unset — AI generation will mark drafts FAILED")
		}

		// Voyage embedder for RAG. nil when the key is absent.
		var emb draft.VoyageEmbedder
		searchDeps := indexing.SearchDeps{Pool: pool}
		if cfg.VoyageAPIKey != "" {
			httpClient := &http.Client{Timeout: 30 * time.Second}
			e, err := indexing.NewVoyageEmbedder(cfg.VoyageAPIKey, cfg.VoyageModel, cfg.VoyageEmbedDim, httpClient)
			if err != nil {
				return fmt.Errorf("init voyage embedder for ai: %w", err)
			}
			emb = e
			logger.Info("AI generation: RAG embedder configured", "model", cfg.VoyageModel)
		} else {
			logger.Warn("VOYAGE_API_KEY unset — AI generation will run without RAG grounding")
		}

		// Publisher pub/sub Redis pro streaming da geração — cada chunk cai
		// no canal draft:<id>:stream e o endpoint SSE do api encaminha ao FE.
		// Parseia direto de cfg.RedisURL (asynq.RedisConnOpt é opaco).
		redisRawOpt, err := redis.ParseURL(cfg.RedisURL)
		if err != nil {
			return fmt.Errorf("parse redis url pra pubsub: %w", err)
		}
		redisClient := redis.NewClient(redisRawOpt)
		chunkPub := pubsub.NewRedisPubSub(redisClient)
		// RAG cache — mesmo Redis client, mesma TTL do api. Se advogado
		// gerou teses e depois pediu Generate na mesma sessão, o cache do
		// api pré-aqueceu a chave; aqui é hit direto (poupa Voyage+pgvector).
		ragCache := draft.NewRAGCache(redisClient, 5*time.Minute)

		generateUC := draft.NewGenerateUseCase(draft.GenerateUseCaseParams{
			UoW:      uow,
			Reader:   repo,
			Writer:   repo,
			Outbox:   outbox,
			Dedup:    dedup,
			Gen:      gen,
			Emb:      emb,
			Search:   searchDeps,
			RAGCache: ragCache,
			Composer: advisory.NewTemplateComposer(),
			ChunkPub: chunkPub,
			// QUALITY: gerar minuta é a peça final que o advogado revisa/assina.
			// Qualidade > velocidade (streaming mascara a latência).
			Model: cfg.OpenRouterModelQuality,
		})

		listener := draft.NewListener(generateUC)
		listener.Register(mux)
		logger.Info("draft generation listener registered")
	}

	if err := srv.Start(mux); err != nil {
		return fmt.Errorf("start asynq server: %w", err)
	}

	stopped := make(chan struct{})
	lifecycle.RunWithGracefulShutdown(
		serviceName,
		func() error {
			<-stopped
			return nil
		},
		func(shutdownCtx context.Context) error {
			srv.Shutdown()
			close(stopped)
			pool.Close()
			if err := telemetryShutdown(shutdownCtx); err != nil {
				return fmt.Errorf("shutdown telemetry: %w", err)
			}
			return nil
		},
	)

	return nil
}

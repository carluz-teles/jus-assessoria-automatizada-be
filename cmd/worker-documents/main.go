// Command worker-documents consumes the "documents" asynq queue (OCR, PDF text
// extraction, document processing). Boot skeleton: same lifecycle as the api —
// config → health → bootstrap → serve — with an empty mux the document feature
// slice fills in. No migrations (§5b.3).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/hibiken/asynq"

	"github.com/jusassessoria/platform/internal/document"
	"github.com/jusassessoria/platform/internal/extraction"
	"github.com/jusassessoria/platform/internal/indexing"
	"github.com/jusassessoria/platform/lib/config"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
	"github.com/jusassessoria/platform/lib/health"
	"github.com/jusassessoria/platform/lib/storage"
	"github.com/jusassessoria/platform/lib/telemetry"
	"github.com/jusassessoria/platform/pkg/lifecycle"
)

const (
	serviceName = "worker-documents"
	queueName   = "documents"
	// concurrency is modest: OCR is CPU/memory-heavy, so fewer parallel jobs
	// than the I/O-bound ingestao worker. Tune with the docker slice.
	concurrency = 5
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

	// Observe wraps EVERY handler (consumer span + failure log), so both pipeline
	// listeners inherit the instrumentation for free.
	mux := asynq.NewServeMux()
	mux.Use(events.Observe(logger))

	// Documentos pipeline (Fatia 5–7) runs here on the "documents" queue: extraction
	// (document.uploaded → PDF text / Claude-vision OCR → document.extracted) and
	// indexing (document.extracted → chunk → Voyage embeddings → document.ready). Both
	// need object storage, so they mount only when S3 is configured (like the api's
	// document handler); without it the worker still boots idle instead of crash-looping.
	if cfg.S3Enabled() {
		storageClient, err := storage.New(ctx, storage.Options{
			Endpoint:  cfg.S3Endpoint,
			Region:    cfg.S3Region,
			Bucket:    cfg.S3Bucket,
			AccessKey: cfg.S3AccessKey,
			SecretKey: cfg.S3SecretKey,
		})
		if err != nil {
			return fmt.Errorf("init storage: %w", err)
		}
		outbox := events.NewOutbox()
		uow := database.NewUnitOfWork(pool)
		// A generous client timeout: the OCR/embedding calls are network-bound and slow.
		httpClient := &http.Client{Timeout: 2 * time.Minute}

		// Fatia 5+6 — text-layer extraction + OCR fallback. OCR_ENGINE picks the OCR
		// adapter: "tesseract" (default) is deterministic and free (the runtime-ocr image
		// ships pdftoppm + tesseract); "claude" is opt-in Claude vision, which uses the
		// Anthropic key (passed through, consumed only when the engine is "claude").
		if err := extraction.RegisterExtractionListeners(mux, extraction.Deps{
			UoW:             uow,
			Storage:         storageClient,
			Outbox:          outbox,
			OCREngine:       cfg.OCREngine,
			AnthropicAPIKey: cfg.AnthropicKey,
			HTTPClient:      httpClient,
			Logger:          logger,
		}); err != nil {
			return fmt.Errorf("register extraction listeners: %w", err)
		}

		// Fatia 7 — chunking + Voyage embeddings + pgvector. The Voyage key is optional
		// config: when unset, skip the indexing listener (extraction still works
		// standalone) rather than crash-loop; when set but invalid, fail boot so the
		// misconfig surfaces instead of silently failing at embed time.
		if cfg.VoyageAPIKey == "" {
			logger.Warn("VOYAGE_API_KEY unset — document indexing listener not registered")
		} else {
			embedder, err := indexing.NewVoyageEmbedder(cfg.VoyageAPIKey, cfg.VoyageModel, cfg.VoyageEmbedDim, httpClient)
			if err != nil {
				return fmt.Errorf("init voyage embedder: %w", err)
			}
			if err := indexing.RegisterIndexingListeners(mux, indexing.Deps{
				UOW:      uow,
				Storage:  storageClient,
				Outbox:   outbox,
				Embedder: embedder,
				EmbedDim: cfg.VoyageEmbedDim,
				Logger:   logger,
			}); err != nil {
				return fmt.Errorf("register indexing listeners: %w", err)
			}
			logger.Info("document indexing listener registered")
		}

		// Terminal lifecycle observer: consumes document.ready / document.failed (which had NO
		// consumer → asynq logged "handler not found" and retried them 10×). It acks them and is
		// the pipeline's success/failure telemetry sink (logs + metrics per outcome/stage).
		observer, err := document.NewLifecycleObserver(logger)
		if err != nil {
			return fmt.Errorf("build document lifecycle observer: %w", err)
		}
		observer.Register(mux)

		logger.Info("documents extraction listener registered", "bucket", cfg.S3Bucket)
	} else {
		logger.Warn("S3 not configured — documents pipeline listeners not registered")
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

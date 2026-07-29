// Command worker-ai consumes the "ai" asynq queue (LLM calls: summaries,
// classification, extraction). Boot skeleton: same lifecycle as the api —
// config → health → bootstrap → serve — with an empty mux the AI feature slice
// fills in. No migrations (§5b.3).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/hibiken/asynq"

	"github.com/jusassessoria/platform/lib/config"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/health"
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
	logger := telemetry.SetupDefault(os.Stdout, slog.LevelInfo)
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

	srv := asynq.NewServer(redisOpt, asynq.Config{
		Concurrency: concurrency,
		Queues:      map[string]int{queueName: concurrency},
	})

	// Feature slices register their listeners on this mux, e.g.
	//   mux.HandleFunc("ai.summary.requested", listener.Handle)
	mux := asynq.NewServeMux()

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

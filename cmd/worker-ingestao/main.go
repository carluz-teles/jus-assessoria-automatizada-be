// Command worker-ingestao consumes the "ingestao" asynq queue (court/process
// sync jobs). This is a boot skeleton: it stands up the same lifecycle as the
// api — config → health → bootstrap → serve — registers the queue, and shuts
// down gracefully. The task handlers are registered here by the acquisition
// feature slice; the mux is intentionally empty for now. No migrations: workers
// assume the api already applied the schema (§5b.3).
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
	serviceName = "worker-ingestao"
	queueName   = "ingestao"
	// concurrency is generous: court sync is I/O-bound and tolerates many
	// in-flight jobs. Tune per real load with the docker slice.
	concurrency = 10
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
	//   mux.HandleFunc("ingestao.sync.requested", listener.Handle)
	// Empty for now: the queue is served, but no task type is handled yet.
	mux := asynq.NewServeMux()

	if err := srv.Start(mux); err != nil {
		return fmt.Errorf("start asynq server: %w", err)
	}

	// srv.Start is non-blocking; block serve until the shutdown hook stops the
	// server, so the lifecycle owns the single signal handler (§5b.1).
	stopped := make(chan struct{})
	lifecycle.RunWithGracefulShutdown(
		serviceName,
		func() error {
			<-stopped
			return nil
		},
		func(shutdownCtx context.Context) error {
			srv.Shutdown() // drains in-flight tasks before returning
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

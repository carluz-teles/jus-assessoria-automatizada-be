// Command worker-outbox-relay drains the transactional outbox into asynq. It is
// the only component that publishes events (§4c.2): a ticker calls events.Relay
// Tick, which reads unpublished rows FOR UPDATE SKIP LOCKED and enqueues them.
// Boot lifecycle mirrors the api minus migrations (§5b.3); the domain logic
// lives entirely in lib/events, so this binary only wires and loops.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/hibiken/asynq"

	"github.com/jusassessoria/platform/lib/config"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
	"github.com/jusassessoria/platform/lib/health"
	"github.com/jusassessoria/platform/lib/telemetry"
	"github.com/jusassessoria/platform/pkg/lifecycle"
)

const (
	serviceName = "worker-outbox-relay"
	// tickInterval bounds the outbox→asynq latency. 1s keeps events near
	// real-time while batching (Tick drains up to 200 rows per pass).
	tickInterval = time.Second
)

func main() {
	logger := telemetry.SetupDefault(os.Stdout, slog.LevelInfo)
	if err := run(logger); err != nil {
		logger.Error("relay boot failed", "error", err)
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
	asynqClient := asynq.NewClient(redisOpt)

	uow := database.NewUnitOfWork(pool)
	relay := events.NewRelay(uow, asynqClient)

	// loopCtx stops the ticker on shutdown; loopDone lets shutdown wait for an
	// in-flight Tick to finish before the pool and client are closed under it.
	loopCtx, cancelLoop := context.WithCancel(context.Background())
	loopDone := make(chan struct{})

	lifecycle.RunWithGracefulShutdown(
		serviceName,
		func() error {
			defer close(loopDone)
			runRelayLoop(loopCtx, logger, relay)
			return nil
		},
		func(shutdownCtx context.Context) error {
			cancelLoop()
			<-loopDone // let the current Tick commit or roll back before teardown

			var errs []error
			pool.Close()
			if err := asynqClient.Close(); err != nil {
				errs = append(errs, fmt.Errorf("close asynq client: %w", err))
			}
			if err := telemetryShutdown(shutdownCtx); err != nil {
				errs = append(errs, fmt.Errorf("shutdown telemetry: %w", err))
			}
			return errors.Join(errs...)
		},
	)

	return nil
}

// runRelayLoop ticks until ctx is cancelled, publishing one outbox batch per
// tick. A Tick failure is logged and the loop continues — the unpublished rows
// stay put and are retried next tick (at-least-once, §4c.2). A single log line
// per non-empty batch keeps the output quiet when the outbox is idle.
func runRelayLoop(ctx context.Context, logger *slog.Logger, relay *events.Relay) {
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			published, err := relay.Tick(ctx)
			if err != nil {
				logger.Error("relay tick failed", "error", err)
				continue
			}
			if published > 0 {
				logger.Info("relay published events", "count", published)
			}
		}
	}
}

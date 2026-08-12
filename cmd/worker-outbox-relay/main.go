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
	"github.com/jusassessoria/platform/lib/obs"
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
	logger := telemetry.SetupDefault(os.Stdout, config.LogLevelFromEnv())
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

	// The relay is a singleton, so it is the one place to observe asynq queue depth
	// without duplicating the series across scaled workers (all see the same Redis).
	inspector := asynq.NewInspector(redisOpt)
	if err := events.RegisterQueueDepth(inspector); err != nil {
		return fmt.Errorf("register queue depth metrics: %w", err)
	}

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
			if err := inspector.Close(); err != nil {
				errs = append(errs, fmt.Errorf("close asynq inspector: %w", err))
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
// stay put and are retried next tick (at-least-once, §4c.2). An idle tick logs
// nothing; a non-empty tick logs one INFO summary broken down by event type
// (which events went out, low cardinality) plus, at DEBUG, one line per event
// correlated to that event's own trace (turn on with LOG_LEVEL=debug to trace a
// single event outbox→consumer without drowning a big backfill in INFO lines).
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
			if len(published) == 0 {
				continue
			}

			byType := make(map[string]int, len(published))
			for _, ev := range published {
				byType[ev.Type]++
				// DEBUG line under the event's own trace, so trace_id/span_id match the
				// producer and the eventual consumer span.
				logger.DebugContext(events.CtxWithTraceContext(ctx, ev.TraceContext),
					"relay published event",
					obs.KeyEventType, ev.Type,
					obs.KeyEventID, ev.ID,
					obs.KeyAggregateID, ev.AggregateID,
				)
			}
			logger.Info("relay published events",
				obs.KeyOutcome, obs.OutcomeOK,
				"count", len(published),
				"by_type", byType,
			)
		}
	}
}

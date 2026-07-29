// Command scheduler is the periodic job trigger: on a fixed interval it will
// enqueue due work (e.g. daily court syncs) onto the asynq queues. Boot
// skeleton — the tick is a placeholder log; the enqueue logic arrives with the
// acquisition feature slice. config → health → tick loop → graceful shutdown.
// No migrations (§5b.3).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jusassessoria/platform/lib/config"
	"github.com/jusassessoria/platform/lib/health"
	"github.com/jusassessoria/platform/lib/telemetry"
	"github.com/jusassessoria/platform/pkg/lifecycle"
)

const (
	serviceName = "scheduler"
	// tickInterval is the cadence at which the scheduler evaluates due work.
	// 60s is a placeholder; the acquisition slice sets the real schedule.
	tickInterval = 60 * time.Second
)

func main() {
	logger := telemetry.SetupDefault(os.Stdout, slog.LevelInfo)
	if err := run(logger); err != nil {
		logger.Error("scheduler boot failed", "error", err)
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

	// loopCtx stops the tick loop on shutdown; loopDone lets shutdown wait for a
	// tick in progress to return before the process exits.
	loopCtx, cancelLoop := context.WithCancel(context.Background())
	loopDone := make(chan struct{})

	lifecycle.RunWithGracefulShutdown(
		serviceName,
		func() error {
			defer close(loopDone)
			runSchedulerLoop(loopCtx, logger)
			return nil
		},
		func(context.Context) error {
			cancelLoop()
			<-loopDone
			return nil
		},
	)

	return nil
}

// runSchedulerLoop ticks until ctx is cancelled. Each tick is a placeholder: the
// acquisition slice replaces the log with "find due syncs and enqueue them".
func runSchedulerLoop(ctx context.Context, logger *slog.Logger) {
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			logger.Info("scheduler tick") // placeholder: enqueue due work here
		}
	}
}

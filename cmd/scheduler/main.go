// Command scheduler is the periodic re-poll trigger: on a fixed interval it scans
// court_record for records whose next_sync_at is due (system-scoped, cross-tenant)
// and enqueues a re-poll for each onto the transactional outbox, which the relay
// publishes and the enrichment consumer refreshes from DATAJUD. config → health →
// pool → tick loop → graceful shutdown. No migrations (§5b.3).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jusassessoria/platform/internal/acquisition"
	"github.com/jusassessoria/platform/lib/config"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
	"github.com/jusassessoria/platform/lib/health"
	"github.com/jusassessoria/platform/lib/telemetry"
	"github.com/jusassessoria/platform/pkg/lifecycle"
)

const (
	serviceName = "scheduler"
	// tickInterval is the cadence at which the scheduler scans for due work. It is
	// the poll granularity, NOT the per-record re-sync cadence (that is the use
	// case's resync interval); a due record is picked up within one tick.
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

	pool, err := database.NewPool(ctx, cfg)
	if err != nil {
		return fmt.Errorf("open database pool: %w", err)
	}

	// The scheduler composes the acquisition re-poll use case: it reads due court
	// records system-scoped and writes re-poll events to the outbox (the relay
	// publishes them). It never touches Redis — enqueueing is the relay's job.
	scheduler := acquisition.NewSchedulerUseCase(
		acquisition.NewRepository(pool),
		events.NewOutbox(),
		database.NewUnitOfWork(pool),
	)

	// loopCtx stops the tick loop on shutdown; loopDone lets shutdown wait for a
	// tick in progress to return before the process exits.
	loopCtx, cancelLoop := context.WithCancel(context.Background())
	loopDone := make(chan struct{})

	lifecycle.RunWithGracefulShutdown(
		serviceName,
		func() error {
			defer close(loopDone)
			runSchedulerLoop(loopCtx, logger, scheduler)
			return nil
		},
		func(context.Context) error {
			cancelLoop()
			<-loopDone
			pool.Close()
			return nil
		},
	)

	return nil
}

// duePoller is the scheduler behavior the loop drives — the acquisition use case
// satisfies it. Depending on the method keeps the loop trivially readable.
type duePoller interface {
	RunDuePoll(ctx context.Context) (int, error)
}

// runSchedulerLoop ticks until ctx is cancelled, running one due-poll per tick. A
// poll error is logged and the loop continues — a transient DB blip must not kill
// the scheduler; the next tick retries.
func runSchedulerLoop(ctx context.Context, logger *slog.Logger, scheduler duePoller) {
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			enqueued, err := scheduler.RunDuePoll(ctx)
			if err != nil {
				logger.ErrorContext(ctx, "scheduler due-poll failed", "error", err)
				continue
			}
			if enqueued > 0 {
				logger.InfoContext(ctx, "scheduler enqueued re-polls", "count", enqueued)
			}
		}
	}
}

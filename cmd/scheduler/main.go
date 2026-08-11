// Command scheduler runs the acquisition slice's periodic loops: (1) the re-poll
// trigger — on a fixed interval it scans court_record for records whose next_sync_at
// is due (system-scoped, cross-tenant) and enqueues a re-poll onto the outbox; and,
// when INGESTION_ENABLED, (2) the national bulk ingestion — once per day it sweeps
// every DJEN tribunal into the publication store and matches it to tenants, plus a
// one-time historical bootstrap (BOOTSTRAP_DAYS). config → health → pool → loops →
// graceful shutdown. No migrations (§5b.3).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/jusassessoria/platform/internal/acquisition"
	"github.com/jusassessoria/platform/lib/calendar"
	"github.com/jusassessoria/platform/lib/config"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
	"github.com/jusassessoria/platform/lib/health"
	"github.com/jusassessoria/platform/lib/telemetry"
	"github.com/jusassessoria/platform/pkg/lifecycle"
)

const (
	serviceName = "scheduler"
	// tickInterval is the cadence at which the re-poll scans for due work.
	tickInterval = 60 * time.Second
	// ingestionTickInterval is how often the daily-ingestion loop wakes to check
	// whether a new calendar day has begun; it runs the day's sweep at most once/day.
	ingestionTickInterval = time.Hour
	// dayLayout is the date-only wire format the publication window uses.
	dayLayout = "2006-01-02"
)

func main() {
	logger := telemetry.SetupDefault(os.Stdout, config.LogLevelFromEnv())
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

	repo := acquisition.NewRepository(pool)
	uow := database.NewUnitOfWork(pool)

	// The re-poll use case reads due court records system-scoped and writes re-poll
	// events to the outbox (the relay publishes them). It never touches Redis.
	scheduler := acquisition.NewSchedulerUseCase(repo, events.NewOutbox(), uow)

	// The national bulk ingestion (the DJEN pivot), built only when enabled so the
	// binary can ship inert and coexist with the per-OAB path until we cut over.
	var ingestion *acquisition.IngestionUseCase
	if cfg.IngestionEnabled {
		connector, cerr := buildDJENConnector(cfg, logger)
		if cerr != nil {
			return cerr
		}
		cal := calendar.New(calendar.NewStore(pool))
		parser := acquisition.ParserSet{acquisition.NewDJENParser(cal), acquisition.NewDATAJUDParser()}
		match := acquisition.NewMatchUseCase(repo, uow, parser)
		ingestion = acquisition.NewIngestionUseCase(connector, repo, uow, match)
		logger.Info("national ingestion enabled", "service", serviceName,
			"lookback_days", cfg.IngestionLookbackDays, "bootstrap_days", cfg.BootstrapDays)
	}

	loopCtx, cancelLoop := context.WithCancel(context.Background())
	loopDone := make(chan struct{})

	lifecycle.RunWithGracefulShutdown(
		serviceName,
		func() error {
			defer close(loopDone)
			var wg sync.WaitGroup

			wg.Add(1)
			go func() { defer wg.Done(); runSchedulerLoop(loopCtx, logger, scheduler) }()

			if ingestion != nil {
				wg.Add(1)
				go func() {
					defer wg.Done()
					runIngestionLoop(loopCtx, logger, ingestion, cfg.IngestionLookbackDays)
				}()
				if cfg.BootstrapDays > 0 {
					wg.Add(1)
					go func() {
						defer wg.Done()
						runBootstrap(loopCtx, logger, ingestion, cfg.BootstrapDays)
					}()
				}
			}

			wg.Wait()
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

// buildDJENConnector composes the DJEN connector from config — the same egress wiring
// as worker-ingestao (residential proxy for the WAF, rate/page overrides). A malformed
// proxy URL fails the boot rather than silently going direct.
func buildDJENConnector(cfg config.Config, logger *slog.Logger) (*acquisition.DJENConnector, error) {
	opts := []acquisition.DJENOption{}
	if cfg.DJENProxyURL != "" {
		proxyURL, perr := url.Parse(cfg.DJENProxyURL)
		if perr != nil {
			return nil, fmt.Errorf("parse DJEN_PROXY_URL: %w", perr)
		}
		opts = append(opts, acquisition.WithDJENProxy(proxyURL))
		logger.Info("DJEN outbound proxy enabled", "service", serviceName, "proxy_host", proxyURL.Host)
	}
	if cfg.DJENRatePerMinute > 0 {
		opts = append(opts, acquisition.WithDJENRatePerMinute(cfg.DJENRatePerMinute))
	}
	if cfg.DJENPageSize > 0 {
		opts = append(opts, acquisition.WithDJENPageSize(cfg.DJENPageSize))
	}
	return acquisition.NewDJENConnector(opts...), nil
}

// duePoller is the re-poll behavior the loop drives — the acquisition use case
// satisfies it. Depending on the method keeps the loop trivially readable.
type duePoller interface {
	RunDuePoll(ctx context.Context) (int, error)
}

// runSchedulerLoop ticks until ctx is cancelled, running one due-poll per tick. A
// poll error is logged and the loop continues — a transient DB blip must not kill the
// scheduler; the next tick retries.
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

// dayIngestor is the ingestion behavior the loops drive (IngestionUseCase).
type dayIngestor interface {
	IngestDay(ctx context.Context, day time.Time) error
}

// runIngestionLoop runs the national sweep once per calendar day: on the first wake of
// a new day it (re-)ingests the lookback window (yesterday back through
// IngestionLookbackDays, to catch late/retracted publications — idempotent). A day
// that fails is not marked done, so the next tick retries the window.
func runIngestionLoop(ctx context.Context, logger *slog.Logger, ingestion dayIngestor, lookback int) {
	ticker := time.NewTicker(ingestionTickInterval)
	defer ticker.Stop()

	var lastRun string
	runOnce := func() {
		today := time.Now().UTC().Format(dayLayout)
		if today == lastRun {
			return
		}
		for _, day := range daysToIngest(time.Now().UTC(), lookback) {
			if ctx.Err() != nil {
				return
			}
			if err := ingestion.IngestDay(ctx, day); err != nil {
				logger.ErrorContext(ctx, "ingestion: daily day failed",
					"day", day.Format(dayLayout), "error", err)
				return // leave lastRun unchanged → retry the whole window next tick
			}
		}
		lastRun = today
		logger.InfoContext(ctx, "ingestion: daily run complete", "through_day", today)
	}

	runOnce() // ingest immediately at boot, not after the first tick
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runOnce()
		}
	}
}

// runBootstrap ingests the last `days` days of the national diário ONCE at boot (the
// cold-start of the store), oldest first, in the background. A failed day is logged
// and skipped — the daily lookback and a re-bootstrap self-heal it.
func runBootstrap(ctx context.Context, logger *slog.Logger, ingestion dayIngestor, days int) {
	logger.InfoContext(ctx, "ingestion: bootstrap starting", "days", days)
	now := time.Now().UTC()
	for i := days; i >= 1; i-- {
		if ctx.Err() != nil {
			return
		}
		day := truncateDay(now.AddDate(0, 0, -i))
		if err := ingestion.IngestDay(ctx, day); err != nil {
			logger.ErrorContext(ctx, "ingestion: bootstrap day failed",
				"day", day.Format(dayLayout), "error", err)
		}
	}
	logger.InfoContext(ctx, "ingestion: bootstrap complete", "days", days)
}

// daysToIngest is the window a daily run (re-)ingests: yesterday back through
// `lookback` days, oldest first (so a process's earlier days land before the newest).
func daysToIngest(now time.Time, lookback int) []time.Time {
	yesterday := truncateDay(now.AddDate(0, 0, -1))
	days := make([]time.Time, 0, lookback+1)
	for i := lookback; i >= 0; i-- {
		days = append(days, yesterday.AddDate(0, 0, -i))
	}
	return days
}

// truncateDay drops the time-of-day, in UTC — the publication window is date-only.
func truncateDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

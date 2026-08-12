// Command scheduler runs the acquisition slice's periodic loops: (1) the re-poll
// trigger — on a fixed interval it scans court_record for records whose next_sync_at
// is due (system-scoped, cross-tenant) and enqueues a re-poll onto the outbox; and,
// when INGESTION_ENABLED, (2) the national bulk ingestion producer — once per day it
// fans every DJEN tribunal into a diario_requested event (worker-ingestao fetches and
// lands each, with per-tribunal retry + DLQ), plus (3) a match tick that folds the
// landed firehose into tenant intimações (idempotent), and a one-time historical
// bootstrap (BOOTSTRAP_DAYS). The scheduler itself never hits DJEN — the fetch moved
// to the worker consumer. config → health → pool → loops → graceful shutdown. No
// migrations (§5b.3).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

	// Without this the scheduler exported NOTHING — no traces, metrics or logs — so the
	// daily import (RequestDay/MatchDay) was invisible in the backend. Setup installs the
	// global providers the loops' spans and the slog OTel bridge delegate to.
	telemetryShutdown, err := telemetry.Setup(ctx, cfg, serviceName)
	if err != nil {
		return fmt.Errorf("setup telemetry: %w", err)
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
	// binary can ship inert and coexist with the per-OAB path until we cut over. The
	// scheduler owns the PRODUCER (fan a day into diario_requested events) and the MATCH
	// tick (fold the landed firehose into tenant intimações); the fetch is the worker's
	// consumer, so no DJEN connector is wired here.
	var requester *acquisition.IngestionScheduler
	var matcher *acquisition.MatchUseCase
	if cfg.IngestionEnabled {
		requester = acquisition.NewIngestionScheduler(events.NewOutbox(), uow)
		// The match re-parses matched payloads through the DJEN parser (holiday calendar
		// for CPC-224 dates); no network egress.
		cal := calendar.New(calendar.NewStore(pool))
		parser := acquisition.ParserSet{acquisition.NewDJENParser(cal), acquisition.NewDATAJUDParser()}
		matcher = acquisition.NewMatchUseCase(repo, uow, parser)
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

			if requester != nil {
				wg.Add(1)
				go func() {
					defer wg.Done()
					runRequestLoop(loopCtx, logger, requester, cfg.IngestionLookbackDays)
				}()
				wg.Add(1)
				go func() {
					defer wg.Done()
					runMatchLoop(loopCtx, logger, matcher, cfg.IngestionLookbackDays)
				}()
				if cfg.BootstrapDays > 0 {
					wg.Add(1)
					go func() {
						defer wg.Done()
						runBootstrap(loopCtx, logger, requester, cfg.BootstrapDays)
					}()
				}
			}

			wg.Wait()
			return nil
		},
		func(shutdownCtx context.Context) error {
			cancelLoop()
			<-loopDone
			var errs []error
			pool.Close()
			if err := telemetryShutdown(shutdownCtx); err != nil {
				errs = append(errs, fmt.Errorf("shutdown telemetry: %w", err))
			}
			return errors.Join(errs...)
		},
	)

	return nil
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

// dayRequester emits the national fetch work for a day (IngestionScheduler.RequestDay).
type dayRequester interface {
	RequestDay(ctx context.Context, day time.Time) (int, error)
}

// dayMatcher folds a day's landed firehose into tenant intimações (MatchUseCase.MatchDay).
type dayMatcher interface {
	MatchDay(ctx context.Context, day time.Time) error
}

// runRequestLoop emits the national fetch work once per calendar day: on the first wake
// of a new day it (re-)requests the lookback window (yesterday back through
// IngestionLookbackDays, to catch late/retracted publications — the events are
// idempotent at the store). A day whose emission fails is not marked done, so the next
// tick retries the whole window. The actual fetch happens asynchronously in the worker
// consumer, so this loop is cheap and returns immediately after enqueueing.
func runRequestLoop(ctx context.Context, logger *slog.Logger, requester dayRequester, lookback int) {
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
			if _, err := requester.RequestDay(ctx, day); err != nil {
				logger.ErrorContext(ctx, "ingestion: request day failed",
					"day", day.Format(dayLayout), "error", err)
				return // leave lastRun unchanged → retry the whole window next tick
			}
		}
		lastRun = today
		logger.InfoContext(ctx, "ingestion: daily request complete", "through_day", today)
	}

	runOnce() // request immediately at boot, not after the first tick
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runOnce()
		}
	}
}

// runMatchLoop folds the landed firehose into tenant intimações on every tick (not
// once/day): because the fetch is asynchronous, publications for a day trickle in over
// minutes/hours behind their diario_requested, so the match re-runs the lookback window
// each tick to pick up whatever has landed since. MatchDay is idempotent (dedup insert +
// upsert), so re-running is safe and just no-ops on already-matched publications. A
// failed day is logged and the loop continues — the next tick retries.
func runMatchLoop(ctx context.Context, logger *slog.Logger, matcher dayMatcher, lookback int) {
	ticker := time.NewTicker(ingestionTickInterval)
	defer ticker.Stop()

	runOnce := func() {
		for _, day := range daysToIngest(time.Now().UTC(), lookback) {
			if ctx.Err() != nil {
				return
			}
			if err := matcher.MatchDay(ctx, day); err != nil {
				logger.ErrorContext(ctx, "ingestion: match day failed",
					"day", day.Format(dayLayout), "error", err)
			}
		}
	}

	runOnce() // match immediately at boot, not after the first tick
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runOnce()
		}
	}
}

// runBootstrap requests the last `days` days of the national diário ONCE at boot (the
// cold-start of the store), oldest first, in the background. It only EMITS the fetch
// work (the worker lands it); a new tenant later matches against the landed history via
// the onboarding cutover (MatchTenantSince). A failed day's emission is logged and
// skipped — the daily lookback and a re-bootstrap self-heal it.
func runBootstrap(ctx context.Context, logger *slog.Logger, requester dayRequester, days int) {
	logger.InfoContext(ctx, "ingestion: bootstrap starting", "days", days)
	now := time.Now().UTC()
	for i := days; i >= 1; i-- {
		if ctx.Err() != nil {
			return
		}
		day := truncateDay(now.AddDate(0, 0, -i))
		if _, err := requester.RequestDay(ctx, day); err != nil {
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

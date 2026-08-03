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

	"github.com/jusassessoria/platform/internal/acquisition"
	"github.com/jusassessoria/platform/internal/billing"
	"github.com/jusassessoria/platform/internal/notifications"
	"github.com/jusassessoria/platform/lib/calendar"
	"github.com/jusassessoria/platform/lib/config"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
	"github.com/jusassessoria/platform/lib/health"
	"github.com/jusassessoria/platform/lib/telemetry"
	"github.com/jusassessoria/platform/pkg/lifecycle"
)

const (
	serviceName = "worker-ingestao"
	queueName   = "ingestao"
	// notificationsQueue carries the avisos domain's `notification.*` events. It
	// shares this worker (the process where the async listeners live) but its own
	// queue, so a slow e-mail send never blocks court sync.
	notificationsQueue = "notifications"
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
		Queues: map[string]int{
			queueName:          concurrency,
			notificationsQueue: concurrency,
		},
	})

	// Feature slices register their listeners on this mux. Each slice owns its
	// task-type registration via a Register(mux) call; the worker only composes.
	mux := asynq.NewServeMux()

	repo := acquisition.NewRepository(pool)
	outbox := events.NewOutbox()
	uow := database.NewUnitOfWork(pool)

	backfill := acquisition.NewBackfillUseCase(repo, outbox, uow)

	// The sync use case resolves its connector per event from the orchestrator, by
	// the event's source. DJEN is the REAL Comunica API connector (national OAB
	// discovery, no auth); DATAJUD stays a stub until its enrichment slice lands.
	orchestrator := acquisition.NewOrchestrator()
	orchestrator.Register(acquisition.SourceDJEN, acquisition.NewDJENConnector())
	orchestrator.Register(acquisition.SourceDATAJUD, acquisition.NewStubConnector(acquisition.SourceDATAJUD))

	// Parsers are resolved by CanParse: the DJEN parser claims DJEN payloads and
	// derives the CPC-224 publication/deadline dates through the judicial calendar
	// (holiday table); the stub parser handles the DATAJUD stub payloads. The DJEN
	// parser goes first so it wins its source.
	cal := calendar.New(calendar.NewStore(pool))
	parser := acquisition.ParserSet{
		acquisition.NewDJENParser(cal),
		acquisition.NewStubParser(),
	}

	// Billing entitlement: the sync cycle gates a NEW court record against the
	// tenant's active_process_limit. acquisition owns the port (EntitlementChecker);
	// billing supplies the adapter over its own repository; this composition root is
	// the ONLY place that knows both slices (they never import each other).
	entitlement := billing.NewEntitlementAdapter(billing.NewRepository(pool))
	sync := acquisition.NewSyncUseCase(repo, outbox, uow, orchestrator, parser,
		acquisition.WithEntitlementChecker(entitlement))

	acquisition.NewListener(backfill, sync).Register(mux)

	// notifications: consume notification.requested and deliver by e-mail (Resend).
	// The provider config is required here (this worker runs the listener), so a
	// missing key fails the boot rather than silently dropping avisos later.
	resendClient, err := notifications.NewResendClient(cfg.ResendAPIKey)
	if err != nil {
		return fmt.Errorf("build resend client: %w", err)
	}
	emailChannel, err := notifications.NewEmailChannel(cfg.ResendFromEmail, resendClient)
	if err != nil {
		return fmt.Errorf("build email channel: %w", err)
	}
	notifyUC := notifications.NewNotifyUseCase(
		notifications.NewRepository(pool),
		emailChannel,
		notifications.NewDedup(),
		uow,
	)
	notifications.NewListener(notifyUC).Register(mux)

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

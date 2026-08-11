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
	"net/url"
	"os"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"

	"github.com/jusassessoria/platform/internal/acquisition"
	"github.com/jusassessoria/platform/internal/notifications"
	"github.com/jusassessoria/platform/lib/calendar"
	"github.com/jusassessoria/platform/lib/config"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
	"github.com/jusassessoria/platform/lib/health"
	"github.com/jusassessoria/platform/lib/pubsub"
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
	// diarioQueue carries the national bulk ingestion (diario_requested). It is drained
	// by a SEPARATE asynq server at diarioConcurrency=1 so the slow, globally-rate-limited
	// DJEN diário fetch runs serialized — concurrency made the 429s WORSE (it trips the
	// endpoint's cumulative cap faster) — and never competes with the enrichment/sync work
	// on the "ingestao" queue. Must match lib/events.queueFor's routing of the event.
	diarioQueue = "diario"
	// diarioConcurrency is fixed at 1: the whole point is to serialize the national fetch
	// against the DJEN global cap. Not env-tunable — a higher value reintroduces the storm.
	diarioConcurrency = 1
	// defaultConcurrency backs INGESTAO_CONCURRENCY when unset/invalid. Parallelism
	// is the backfill's main lever: DJEN/DATAJUD fetches are LONG (~110s each, stuck
	// on the residential proxy), so running many at once overlaps that wait and cuts
	// the wall clock ~N×. The concurrent writes this unlocks are serialized per tenant
	// by the AcquireTenantWriteLock advisory lock, so there is no 40P01 deadlock.
	defaultConcurrency = 8
	// onboardingHistoryDays is how far back a new tenant is caught up from the stored
	// firehose on the cutover — matched to the publication store's ~90d retention.
	onboardingHistoryDays = 90
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

	concurrency := cfg.IngestaoConcurrency
	if concurrency <= 0 {
		concurrency = defaultConcurrency
	}
	logger.Info("worker concurrency", "service", serviceName, "concurrency", concurrency)

	// asynq's own logs go through slog (structured, OTLP); LogLevel=ErrorLevel keeps
	// its per-task retry Warns from duplicating the Observe middleware's failure log.
	// The MAIN server drains the enrichment/sync ("ingestao") and avisos
	// ("notifications") queues; the national bulk ingestion runs on its own server below.
	srv := asynq.NewServer(redisOpt, asynq.Config{
		Concurrency: concurrency,
		Queues: map[string]int{
			queueName:          concurrency,
			notificationsQueue: concurrency,
		},
		Logger:   events.NewAsynqLogger(logger),
		LogLevel: asynq.ErrorLevel,
	})

	// Feature slices register their listeners on this mux. Each slice owns its
	// task-type registration via a Register(mux) call; the worker only composes.
	// Observe wraps EVERY handler: consumer span + failure log, one place.
	mux := asynq.NewServeMux()
	mux.Use(events.Observe(logger))

	repo := acquisition.NewRepository(pool)
	outbox := events.NewOutbox()
	uow := database.NewUnitOfWork(pool)

	// The sync use case resolves its connector per event from the orchestrator, by
	// the event's source. Both connectors are REAL now: DJEN discovers nationally by
	// OAB (no auth); DATAJUD enriches one process by number from the tribunal's index
	// (public API key). DATAJUD never discovers, so it is not an activatable
	// integration — it runs only through the enrichment listener below.
	orchestrator := acquisition.NewOrchestrator()

	// DJEN's Comunica WAF 403s the datacenter egress IP; when DJEN_PROXY_URL is set,
	// route the connector through a residential/BR proxy for a clean IP. Unset = a
	// direct connection (dev local passes without it). A malformed URL fails the boot.
	djenOpts := []acquisition.DJENOption{}
	if cfg.DJENProxyURL != "" {
		proxyURL, perr := url.Parse(cfg.DJENProxyURL)
		if perr != nil {
			return fmt.Errorf("parse DJEN_PROXY_URL: %w", perr)
		}
		djenOpts = append(djenOpts, acquisition.WithDJENProxy(proxyURL))
		logger.Info("DJEN outbound proxy enabled", "service", serviceName, "proxy_host", proxyURL.Host)
	}
	// A gentler pace than the connector's 1 req/s default, dialable per env when a big
	// OAB sweep 429s the DJEN egress (0 = keep the default).
	if cfg.DJENRatePerMinute > 0 {
		djenOpts = append(djenOpts, acquisition.WithDJENRatePerMinute(cfg.DJENRatePerMinute))
		logger.Info("DJEN rate override", "service", serviceName, "rate_per_minute", cfg.DJENRatePerMinute)
	}
	// Page size override (0 = keep the connector default of 1000). A bigger page means
	// fewer requests per window against the 1 req/s pace.
	if cfg.DJENPageSize > 0 {
		djenOpts = append(djenOpts, acquisition.WithDJENPageSize(cfg.DJENPageSize))
		logger.Info("DJEN page size override", "service", serviceName, "page_size", cfg.DJENPageSize)
	}
	// One DJEN connector instance backs both the per-OAB sync (via the orchestrator) and
	// the national ingestion consumer (FetchDiario), so they share the same egress: proxy,
	// rate limiter and page size. The national fetch is now the worker's job (it moved off
	// the scheduler in the event-driven ingestion redesign).
	djenConnector := acquisition.NewDJENConnector(djenOpts...)
	orchestrator.Register(acquisition.SourceDJEN, djenConnector)
	orchestrator.Register(acquisition.SourceDATAJUD, acquisition.NewDATAJUDConnector())

	// Parsers are resolved by CanParse: the DJEN parser claims DJEN payloads and
	// derives the CPC-224 publication/deadline dates through the judicial calendar
	// (holiday table); the DATAJUD parser claims DATAJUD payloads and maps the graded
	// process + its movimentos.
	cal := calendar.New(calendar.NewStore(pool))
	parser := acquisition.ParserSet{
		acquisition.NewDJENParser(cal),
		acquisition.NewDATAJUDParser(),
	}

	// Backfill onboarding, built after the parser so the cutover can wire the match.
	// The window is dialed from BACKFILL_WINDOW_DAYS. When INGESTION_ENABLED, the
	// cutover swaps the per-OAB backfill for a history catch-up against the stored
	// firehose (MatchTenantSince over the retention window) — a new tenant sees its
	// recent intimações immediately with zero per-OAB DJEN calls, and the scheduler's
	// daily match covers everything forward. Off = the legacy per-OAB backfill.
	// ingestion is the national bulk consumer (diario_requested → FetchDiario + land),
	// built only when the pivot is on; nil keeps the listener inert to the event.
	var ingestion *acquisition.IngestionUseCase
	var backfill *acquisition.BackfillUseCase
	if cfg.IngestionEnabled {
		ingestion = acquisition.NewIngestionUseCase(djenConnector, repo, uow)
		match := acquisition.NewMatchUseCase(repo, uow, parser)
		backfill = acquisition.NewBackfillUseCase(repo, outbox, uow,
			acquisition.WithBackfillWindowDays(cfg.BackfillWindowDays),
			acquisition.WithHistoryMatcher(match, onboardingHistoryDays),
		)
		logger.Info("onboarding cutover enabled (history match, no per-OAB backfill)",
			"service", serviceName, "history_days", onboardingHistoryDays)
	} else {
		backfill = acquisition.NewBackfillUseCase(repo, outbox, uow,
			acquisition.WithBackfillWindowDays(cfg.BackfillWindowDays),
		)
	}
	if cfg.BackfillWindowDays > 0 {
		logger.Info("backfill window override", "service", serviceName, "window_days", cfg.BackfillWindowDays)
	}

	// TEMP (E2E do bulk): gate de entitlement DESLIGADO — o SyncUseCase cai no default
	// sem ceiling (unlimitedEntitlement), pra o onboarding de teste rodar sem
	// subscription. RESTAURAR depois do E2E:
	//   entitlement := billing.NewEntitlementAdapter(billing.NewRepository(pool))
	//   sync := acquisition.NewSyncUseCase(..., acquisition.WithEntitlementChecker(entitlement))
	sync := acquisition.NewSyncUseCase(repo, outbox, uow, orchestrator, parser)

	// DATAJUD enrichment reacts to court_record_observed (a DJEN placeholder,
	// degree=UNKNOWN): it fetches the process by number to reveal the grau and does
	// the placeholder+merge. It shares the orchestrator and the ParserSet.
	enrichment := acquisition.NewEnrichmentUseCase(repo, outbox, uow, orchestrator, parser)

	// The listener mounts the shared enrichment/sync/backfill handlers on the MAIN mux.
	// diario_requested is NOT among them — it runs on the dedicated server below. Register
	// never dereferences the ingestion dep, so a nil (pivot off) is harmless here.
	listener := acquisition.NewListener(backfill, sync, enrichment, ingestion)
	listener.Register(mux)

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
	// The two notifications use cases share a pool-backed repo and the stateless dedup:
	// the email consumer (notification.requested) and the in-app consumer (slice 1a),
	// which turns acquisition's backfill_finished/docket_entry_observed into IN_APP
	// avisos. Registering the in-app handlers here is what lets acquisition drop its
	// drainUnconsumed placeholder for those two types (one handler per type on the mux).
	// A dedicated redis client for the best-effort in-app push (slice 2b): when the
	// in-app consumer persists an aviso it publishes it on notif:<tenant> for the SSE
	// endpoint (a later slice) to relay in real time. It shares the same Redis as asynq
	// but its own client, closed in the graceful shutdown below.
	redisClientOpt, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return fmt.Errorf("parse redis url for pub/sub: %w", err)
	}
	pubsubClient := redis.NewClient(redisClientOpt)

	notifRepo := notifications.NewRepository(pool)
	notifDedup := notifications.NewDedup()
	notifyUC := notifications.NewNotifyUseCase(notifRepo, emailChannel, notifDedup, uow)
	inAppUC := notifications.NewInAppUseCase(notifRepo, notifDedup, uow, pubsub.NewRedisPubSub(pubsubClient))
	notifications.NewListener(notifyUC, inAppUC).Register(mux)

	if err := srv.Start(mux); err != nil {
		return fmt.Errorf("start asynq server: %w", err)
	}

	// The DEDICATED national-ingestion server: one worker, its own "diario" queue, so the
	// slow globally-rate-limited DJEN diário fetch runs serialized (never 3-way hammering
	// the cumulative cap) and stays off the enrichment/sync queue. Built only when the
	// pivot is on; its own mux carries just the diario_requested handler.
	var diarioSrv *asynq.Server
	if ingestion != nil {
		diarioSrv = asynq.NewServer(redisOpt, asynq.Config{
			Concurrency: diarioConcurrency,
			Queues:      map[string]int{diarioQueue: diarioConcurrency},
			Logger:      events.NewAsynqLogger(logger),
			LogLevel:    asynq.ErrorLevel,
		})
		diarioMux := asynq.NewServeMux()
		diarioMux.Use(events.Observe(logger))
		listener.RegisterIngestion(diarioMux)
		if err := diarioSrv.Start(diarioMux); err != nil {
			return fmt.Errorf("start diario asynq server: %w", err)
		}
		logger.Info("national ingestion server started (dedicated, serialized)",
			"service", serviceName, "queue", diarioQueue, "concurrency", diarioConcurrency)
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
			if diarioSrv != nil {
				diarioSrv.Shutdown()
			}
			close(stopped)
			if err := pubsubClient.Close(); err != nil {
				logger.Warn("close pub/sub redis client", "service", serviceName, "error", err)
			}
			pool.Close()
			if err := telemetryShutdown(shutdownCtx); err != nil {
				return fmt.Errorf("shutdown telemetry: %w", err)
			}
			return nil
		},
	)

	return nil
}
